package bruteforce

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Index is a simple brute-force vector index implementing cosine similarity.
type Index struct {
	ids  []string
	vecs [][]float32
	dim  int
	mags []float64
}

// Build loads ids and vectors and precomputes magnitudes.
func (i *Index) Build(ids []string, vectors [][]float32) error {
	if len(ids) != len(vectors) {
		return fmt.Errorf("bruteforce: ids and vectors length mismatch: %d != %d", len(ids), len(vectors))
	}
	if len(ids) == 0 {
		i.ids, i.vecs, i.mags, i.dim = nil, nil, nil, 0
		return nil
	}
	dim := len(vectors[0])
	for j := range vectors {
		if len(vectors[j]) != dim {
			return fmt.Errorf("bruteforce: inconsistent vector dims %d vs %d", len(vectors[j]), dim)
		}
	}
	mags := make([]float64, len(vectors))
	for j := range vectors {
		mags[j] = magnitude(vectors[j])
	}
	i.ids = append([]string(nil), ids...)
	i.vecs = append([][]float32(nil), vectors...)
	i.dim = dim
	i.mags = mags
	return nil
}

// Query returns top-k by cosine similarity.
func (i *Index) Query(query []float32, k int) ([]string, []float64, error) {
	if i.dim == 0 || len(i.vecs) == 0 {
		return nil, nil, nil
	}
	if len(query) != i.dim {
		return nil, nil, fmt.Errorf("bruteforce: query dim %d != index dim %d", len(query), i.dim)
	}
	qm := magnitude(query)
	if qm == 0 {
		return nil, nil, nil
	}
	type scored struct {
		idx   int
		score float64
	}
	scoreds := make([]scored, 0, len(i.vecs))
	for j := range i.vecs {
		if i.mags[j] == 0 {
			continue
		}
		s := dot(query, i.vecs[j]) / (qm * i.mags[j])
		if math.IsNaN(s) {
			continue
		}
		scoreds = append(scoreds, scored{idx: j, score: s})
	}
	sort.Slice(scoreds, func(a, b int) bool { return scoreds[a].score > scoreds[b].score })
	if k <= 0 || k > len(scoreds) {
		k = len(scoreds)
	}
	outIDs := make([]string, k)
	outScores := make([]float64, k)
	for n := 0; n < k; n++ {
		outIDs[n] = i.ids[scoreds[n].idx]
		outScores[n] = scoreds[n].score
	}
	return outIDs, outScores, nil
}

// MarshalBinary stores: dim(uint32), n(uint32), then for each item:
// idLen(uint32), id bytes, vec(float32[dim]).
func (i *Index) MarshalBinary() ([]byte, error) {
	if i.dim == 0 || len(i.vecs) == 0 {
		// encode empty
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint32(buf[0:4], uint32(0))
		binary.LittleEndian.PutUint32(buf[4:8], uint32(0))
		return buf, nil
	}
	// Rough size estimate
	size := 8
	for _, id := range i.ids {
		size += 4 + len(id) + 4*i.dim
	}
	out := make([]byte, 0, size)
	putU32 := func(v uint32) { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); out = append(out, b...) }
	putF32 := func(v float32) {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, math.Float32bits(v))
		out = append(out, b...)
	}
	putU32(uint32(i.dim))
	putU32(uint32(len(i.ids)))
	for idx, id := range i.ids {
		putU32(uint32(len(id)))
		out = append(out, []byte(id)...)
		vec := i.vecs[idx]
		for j := 0; j < i.dim; j++ {
			putF32(vec[j])
		}
	}
	return out, nil
}

// UnmarshalBinary restores the index from bytes.
func (i *Index) UnmarshalBinary(data []byte) error {
	if len(data) < 8 {
		return errors.New("bruteforce: invalid data")
	}
	off := 0
	getU32 := func() uint32 { v := binary.LittleEndian.Uint32(data[off : off+4]); off += 4; return v }
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
			return errors.New("bruteforce: truncated")
		}
		idlen := int(getU32())
		if off+idlen > len(data) {
			return errors.New("bruteforce: truncated id")
		}
		ids[idx] = string(data[off : off+idlen])
		off += idlen
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			if off+4 > len(data) {
				return errors.New("bruteforce: truncated vec")
			}
			vec[j] = getF32()
		}
		vecs[idx] = vec
	}
	return i.Build(ids, vecs)
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
func magnitude(v []float32) float64 { return math.Sqrt(dot(v, v)) }
