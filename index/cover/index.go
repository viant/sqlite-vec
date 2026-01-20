package cover

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"

	"github.com/viant/sqlite-vec/index/bruteforce"
	ctree "github.com/viant/sqlite-vec/internal/cover/tree"
	"github.com/viant/vec/search"
)

const (
	defaultBase   = 1.3
	coverMagic    = "COV1"
	coverVersion1 = uint32(1)
)

// BoundStrategy exposes the pruning strategy knobs for cover-tree searches.
type BoundStrategy int

const (
	// BoundPerNode caches subtree radii per node (tighter pruning, more bookkeeping).
	BoundPerNode BoundStrategy = iota
	// BoundLevel uses level-derived bounds (looser but cheaper to evaluate).
	BoundLevel
)

// Option configures a cover Index instance.
type Option func(*Index)

// Index wraps a cover tree (adapted from viant/gds) behind the sqlite-vec index API.
type Index struct {
	base          float32
	dim           int
	ids           []string
	vecs          [][]float32
	tree          *ctree.Tree[string]
	buildWorkers  int
	boundStrategy BoundStrategy
	distance      ctree.DistanceFunction
}

// New constructs an Index with optional tuning knobs.
func New(opts ...Option) *Index {
	idx := &Index{}
	for _, opt := range opts {
		if opt != nil {
			opt(idx)
		}
	}
	return idx
}

// WithBase overrides the geometric base used by the cover tree (default 1.3).
func WithBase(base float32) Option { return func(i *Index) { i.base = base } }

// WithBuildParallelism sets the number of workers used to clone vectors/magnitudes during Build.
// Zero/negative values fall back to GOMAXPROCS with automatic throttling for small datasets.
func WithBuildParallelism(workers int) Option { return func(i *Index) { i.buildWorkers = workers } }

// WithBoundStrategy selects the pruning strategy used during queries.
func WithBoundStrategy(strategy BoundStrategy) Option {
	return func(i *Index) { i.boundStrategy = strategy }
}

// DistanceFunction re-exports cover-tree distance identifiers for callers.
type DistanceFunction = ctree.DistanceFunction

const (
	// DistanceFunctionCosine uses cosine distance (default).
	DistanceFunctionCosine DistanceFunction = ctree.DistanceFunctionCosine
	// DistanceFunctionEuclidean uses Euclidean distance.
	DistanceFunctionEuclidean DistanceFunction = ctree.DistanceFunctionEuclidean
)

// WithDistance switches the cover-tree distance metric (default cosine).
func WithDistance(distance DistanceFunction) Option {
	return func(i *Index) { i.distance = ctree.DistanceFunction(distance) }
}

// Build constructs the cover tree from ids/vectors using cosine distance.
func (i *Index) Build(ids []string, vectors [][]float32) error {
	if len(ids) != len(vectors) {
		return errors.New("cover: ids/vectors length mismatch")
	}
	if len(ids) == 0 {
		i.ids, i.vecs, i.tree, i.dim = nil, nil, nil, 0
		i.ensureTree()
		return nil
	}
	dim := len(vectors[0])
	for idx := range vectors {
		if len(vectors[idx]) != dim {
			return errors.New("cover: inconsistent dims")
		}
	}
	i.dim = dim
	i.ensureTree()

	copiedIDs := make([]string, len(ids))
	copiedVecs := make([][]float32, len(ids))
	workers := i.workerCount(len(ids))
	if workers <= 1 {
		for idx := range ids {
			vecCopy := append([]float32(nil), vectors[idx]...)
			point := &ctree.Point{
				Vector:    vecCopy,
				Magnitude: search.Float32s(vecCopy).Magnitude(),
			}
			i.tree.Insert(ids[idx], point)
			copiedIDs[idx] = ids[idx]
			copiedVecs[idx] = vecCopy
		}
		i.ids = copiedIDs
		i.vecs = copiedVecs
		return nil
	}
	points := make([]*ctree.Point, len(ids))
	i.prepareParallelPoints(workers, ids, vectors, copiedIDs, copiedVecs, points)
	for idx := range points {
		i.tree.Insert(copiedIDs[idx], points[idx])
	}
	i.ids = copiedIDs
	i.vecs = copiedVecs
	return nil
}

// Query performs a cosine kNN search using the underlying cover tree.
func (i *Index) Query(query []float32, k int) ([]string, []float64, error) {
	if i.tree == nil || i.dim == 0 || len(i.ids) == 0 {
		return nil, nil, nil
	}
	if len(query) != i.dim {
		return nil, nil, errors.New("cover: query dim mismatch")
	}
	mag := search.Float32s(query).Magnitude()
	if mag == 0 {
		return nil, nil, nil
	}
	if k <= 0 || k > len(i.ids) {
		k = len(i.ids)
	}
	point := &ctree.Point{
		Vector:    query,
		Magnitude: mag,
	}
	neighbors := i.tree.KNearestNeighborsBestFirst(point, k)
	if len(neighbors) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, len(neighbors))
	scores := make([]float64, len(neighbors))
	for idx, n := range neighbors {
		ids[idx] = i.tree.Value(n.Point)
		scores[idx] = clampCosine(1 - float64(n.Distance))
	}
	return ids, scores, nil
}

// MarshalBinary persists the cover tree along with document values.
func (i *Index) MarshalBinary() ([]byte, error) {
	if i.tree == nil {
		bf := &bruteforce.Index{}
		if err := bf.Build(i.ids, i.vecs); err != nil {
			return nil, err
		}
		return bf.MarshalBinary()
	}
	var buf bytes.Buffer
	buf.WriteString(coverMagic)
	if err := binary.Write(&buf, binary.LittleEndian, coverVersion1); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, uint32(i.dim)); err != nil {
		return nil, err
	}
	if err := i.tree.EncodeBinary(&buf, func(w io.Writer, value string) error {
		return writeStringValue(w, value)
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalBinary restores the index from the cover-tree format or the legacy brute format.
func (i *Index) UnmarshalBinary(data []byte) error {
	if len(data) >= len(coverMagic) && string(data[:len(coverMagic)]) == coverMagic {
		reader := bytes.NewReader(data[len(coverMagic):])
		var version uint32
		if err := binary.Read(reader, binary.LittleEndian, &version); err != nil {
			return err
		}
		var dim uint32
		if err := binary.Read(reader, binary.LittleEndian, &dim); err != nil {
			return err
		}
		if i.tree == nil {
			i.tree = ctree.NewTree[string](defaultBase, ctree.DistanceFunctionCosine)
		}
		if err := i.tree.DecodeBinary(reader, readStringValue); err != nil {
			return err
		}
		i.tree.SetBoundStrategy(i.resolveBoundStrategy())
		i.dim = int(dim)
		i.refreshCachesFromTree()
		return nil
	}

	bf := &bruteforce.Index{}
	if err := bf.UnmarshalBinary(data); err != nil {
		return err
	}
	ids, vecs, err := decodeBruteData(data)
	if err != nil {
		return err
	}
	return i.Build(ids, vecs)
}

func (i *Index) ensureTree() {
	if i.base <= 1 {
		i.base = defaultBase
	}
	if i.distance == "" {
		i.distance = ctree.DistanceFunctionCosine
	}
	i.tree = ctree.NewTree[string](i.base, i.distance)
	i.tree.SetBoundStrategy(i.resolveBoundStrategy())
}

func (i *Index) resolveBoundStrategy() ctree.BoundStrategy {
	switch i.boundStrategy {
	case BoundLevel:
		return ctree.BoundLevel
	default:
		return ctree.BoundPerNode
	}
}

func (i *Index) prepareParallelPoints(workers int, ids []string, vectors [][]float32, copiedIDs []string, copiedVecs [][]float32, points []*ctree.Point) {
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				vecCopy := append([]float32(nil), vectors[idx]...)
				copiedIDs[idx] = ids[idx]
				copiedVecs[idx] = vecCopy
				points[idx] = &ctree.Point{
					Vector:    vecCopy,
					Magnitude: search.Float32s(vecCopy).Magnitude(),
				}
			}
		}()
	}
	for idx := range ids {
		jobs <- idx
	}
	close(jobs)
	wg.Wait()
}

func (i *Index) workerCount(total int) int {
	workers := i.buildWorkers
	if workers <= 1 || total < 2 {
		return 1
	}
	if workers > total {
		return total
	}
	return workers
}

func (i *Index) refreshCachesFromTree() {
	if i.tree == nil {
		i.ids = nil
		i.vecs = nil
		i.dim = 0
		i.distance = ""
		return
	}
	var ids []string
	var vecs [][]float32
	i.tree.ForEach(func(value string, point *ctree.Point) {
		ids = append(ids, value)
		vecCopy := append([]float32(nil), point.Vector...)
		vecs = append(vecs, vecCopy)
	})
	i.ids = ids
	i.vecs = vecs
	i.distance = i.tree.DistanceFunction()
	if len(vecs) > 0 {
		i.dim = len(vecs[0])
	} else {
		i.dim = 0
	}
}

func clampCosine(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// decodeBruteData decodes the brute-force binary format.
func decodeBruteData(data []byte) ([]string, [][]float32, error) {
	if len(data) < 8 {
		return nil, nil, errors.New("cover: invalid data")
	}
	off := 0
	getU32 := func() uint32 {
		v := binary.LittleEndian.Uint32(data[off : off+4])
		off += 4
		return v
	}
	getF32 := func() float32 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
		off += 4
		return v
	}
	dim := int(getU32())
	n := int(getU32())
	ids := make([]string, n)
	vecs := make([][]float32, n)
	for idx := 0; idx < n; idx++ {
		if off+4 > len(data) {
			return nil, nil, errors.New("cover: truncated")
		}
		idlen := int(getU32())
		if off+idlen > len(data) {
			return nil, nil, errors.New("cover: truncated id")
		}
		ids[idx] = string(data[off : off+idlen])
		off += idlen
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			if off+4 > len(data) {
				return nil, nil, errors.New("cover: truncated vec")
			}
			vec[j] = getF32()
		}
		vecs[idx] = vec
	}
	return ids, vecs, nil
}

func writeStringValue(w io.Writer, value string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(value))); err != nil {
		return err
	}
	if len(value) == 0 {
		return nil
	}
	_, err := io.WriteString(w, value)
	return err
}

func readStringValue(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
