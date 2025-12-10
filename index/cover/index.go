package cover

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/viant/sqlite-vec/index/bruteforce"
	ctree "github.com/viant/sqlite-vec/internal/cover/tree"
	"github.com/viant/vec/search"
)

const defaultBase = 1.3

// Index wraps a cover tree (adapted from viant/gds) behind the sqlite-vec index API.
type Index struct {
	base float32
	dim  int
	ids  []string
	vecs [][]float32
	tree *ctree.Tree[string]
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

// MarshalBinary uses the brute-force format for persistence (compatibility).
func (i *Index) MarshalBinary() ([]byte, error) {
	bf := &bruteforce.Index{}
	if err := bf.Build(i.ids, i.vecs); err != nil {
		return nil, err
	}
	return bf.MarshalBinary()
}

// UnmarshalBinary restores the index from the brute-force format.
func (i *Index) UnmarshalBinary(data []byte) error {
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
	i.tree = ctree.NewTree[string](i.base, ctree.DistanceFunctionCosine)
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
