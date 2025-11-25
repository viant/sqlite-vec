package cover

import (
    "encoding/binary"
    "errors"
    "math"
    "sort"

    "github.com/viant/sqlite-vec/index/bruteforce"
)

// Index implements a cosine kNN index using a VP-tree to prune search.
// It serializes/deserializes using the brute-force encoding for compatibility.
type Index struct {
    ids  []string
    vecs [][]float32
    mags []float64
    dim  int
    root *node
}

type node struct {
    idx      int    // index into ids/vecs
    thr      float64
    left     *node
    right    *node
}

// Build constructs the VP-tree and caches magnitudes.
func (i *Index) Build(ids []string, vectors [][]float32) error {
    if len(ids) != len(vectors) { return errors.New("cover: ids/vectors length mismatch") }
    i.ids = append([]string(nil), ids...)
    i.vecs = append([][]float32(nil), vectors...)
    i.mags = make([]float64, len(vectors))
    if len(vectors) == 0 { i.dim = 0; i.root = nil; return nil }
    i.dim = len(vectors[0])
    for j := range vectors {
        if len(vectors[j]) != i.dim { return errors.New("cover: inconsistent dims") }
        i.mags[j] = magnitude(vectors[j])
    }
    idxs := make([]int, len(vectors))
    for k := range idxs { idxs[k] = k }
    i.root = i.buildVP(idxs)
    return nil
}

func (i *Index) buildVP(idxs []int) *node {
    if len(idxs) == 0 { return nil }
    // pick last as vantage point to avoid extra randomness
    vp := idxs[len(idxs)-1]
    idxs = idxs[:len(idxs)-1]
    if len(idxs) == 0 { return &node{idx: vp} }
    // compute distances to vp
    dists := make([]float64, len(idxs))
    for k, j := range idxs {
        dists[k] = 1.0 - cosine(i.vecs[vp], i.mags[vp], i.vecs[j], i.mags[j])
    }
    // median threshold
    mid := len(dists)/2
    order := make([]int, len(idxs))
    for k := range order { order[k] = k }
    sort.Slice(order, func(a,b int) bool { return dists[order[a]] < dists[order[b]] })
    thr := dists[order[mid]]
    leftIdxs := make([]int, 0, mid+1)
    rightIdxs := make([]int, 0, len(idxs)-(mid+1))
    for rank, k := range order {
        if rank <= mid { leftIdxs = append(leftIdxs, idxs[k]) } else { rightIdxs = append(rightIdxs, idxs[k]) }
    }
    return &node{
        idx:   vp,
        thr:   thr,
        left:  i.buildVP(leftIdxs),
        right: i.buildVP(rightIdxs),
    }
}

// Query returns up to k ids ordered by decreasing cosine similarity.
func (i *Index) Query(query []float32, k int) ([]string, []float64, error) {
    if i.dim == 0 || len(i.vecs) == 0 { return nil, nil, nil }
    if len(query) != i.dim { return nil, nil, errors.New("cover: query dim mismatch") }
    qm := magnitude(query)
    if qm == 0 { return nil, nil, nil }
    // max-heap by distance (we keep smallest distance), we’ll convert to scores
    type cand struct { idx int; dist float64 }
    heap := make([]cand, 0, max(1, k))
    bestR := math.Inf(1)
    var search func(n *node)
    search = func(n *node) {
        if n == nil { return }
        d := 1.0 - cosine(query, qm, i.vecs[n.idx], i.mags[n.idx])
        // push candidate
        if k <= 0 {
            if d < bestR { bestR = d }
        } else {
            if len(heap) < k {
                heap = append(heap, cand{idx: n.idx, dist: d})
                if len(heap) == k { // establish current bound
                    // find current worst (max dist)
                    worst := 0
                    for t := 1; t < len(heap); t++ { if heap[t].dist > heap[worst].dist { worst = t } }
                    bestR = heap[worst].dist
                }
            } else if d < bestR {
                // replace current worst
                worst := 0
                for t := 1; t < len(heap); t++ { if heap[t].dist > heap[worst].dist { worst = t } }
                heap[worst] = cand{idx: n.idx, dist: d}
                // recompute worst
                worst = 0
                for t := 1; t < len(heap); t++ { if heap[t].dist > heap[worst].dist { worst = t } }
                bestR = heap[worst].dist
            }
        }
        // prune using triangle inequality
        if d < n.thr {
            // search left first
            if d-bestR <= n.thr { search(n.left) }
            if d+bestR >= n.thr { search(n.right) }
        } else {
            if d+bestR >= n.thr { search(n.right) }
            if d-bestR <= n.thr { search(n.left) }
        }
    }
    search(i.root)
    // sort results by similarity descending
    if k <= 0 { k = len(heap) }
    if k > len(heap) { k = len(heap) }
    sort.Slice(heap, func(a,b int) bool {
        // similarity = 1 - dist
        return (1.0 - heap[a].dist) > (1.0 - heap[b].dist)
    })
    ids := make([]string, k)
    scores := make([]float64, k)
    for n := 0; n < k; n++ {
        ids[n] = i.ids[heap[n].idx]
        scores[n] = 1.0 - heap[n].dist
    }
    return ids, scores, nil
}

// MarshalBinary uses the brute-force format for persistence.
func (i *Index) MarshalBinary() ([]byte, error) {
    bf := &bruteforce.Index{}
    if err := bf.Build(i.ids, i.vecs); err != nil { return nil, err }
    return bf.MarshalBinary()
}

// UnmarshalBinary loads brute-force format and rebuilds the VP-tree.
func (i *Index) UnmarshalBinary(data []byte) error {
    bf := &bruteforce.Index{}
    if err := bf.UnmarshalBinary(data); err != nil { return err }
    // Parse inline according to brute-force format: dim(uint32), n(uint32), [idlen(uint32), id, vec(float32[dim])]^n
    ids, vecs, err := decodeBruteData(data)
    if err != nil { return err }
    return i.Build(ids, vecs)
}

// helpers
func dot(a, b []float32) float64 { var s float64; for i := range a { s += float64(a[i]) * float64(b[i]) }; return s }
func magnitude(v []float32) float64 { return math.Sqrt(dot(v, v)) }
func cosine(a []float32, am float64, b []float32, bm float64) float64 {
    if am == 0 || bm == 0 { return 0 }
    return dot(a,b) / (am * bm)
}
func max(a,b int) int { if a>b { return a }; return b }

// decodeBruteData decodes the brute-force binary format.
func decodeBruteData(data []byte) ([]string, [][]float32, error) {
    if len(data) < 8 { return nil, nil, errors.New("cover: invalid data") }
    off := 0
    getU32 := func() uint32 { v := binary.LittleEndian.Uint32(data[off:off+4]); off += 4; return v }
    getF32 := func() float32 { v := math.Float32frombits(binary.LittleEndian.Uint32(data[off:off+4])); off += 4; return v }
    dim := int(getU32())
    n := int(getU32())
    ids := make([]string, n)
    vecs := make([][]float32, n)
    for idx := 0; idx < n; idx++ {
        if off+4 > len(data) { return nil, nil, errors.New("cover: truncated") }
        idlen := int(getU32())
        if off+idlen > len(data) { return nil, nil, errors.New("cover: truncated id") }
        ids[idx] = string(data[off:off+idlen])
        off += idlen
        vec := make([]float32, dim)
        for j := 0; j < dim; j++ {
            if off+4 > len(data) { return nil, nil, errors.New("cover: truncated vec") }
            vec[j] = getF32()
        }
        vecs[idx] = vec
    }
    return ids, vecs, nil
}
