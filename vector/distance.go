package vector

import (
	"fmt"
	"math"
)

// CosineSimilarity computes the cosine similarity between two vectors. It
// returns an error if the vectors have different lengths or if either vector
// has zero magnitude.
func CosineSimilarity(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vector: cosine similarity dimension mismatch: %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		return 0, fmt.Errorf("vector: cosine similarity on empty vectors")
	}
	var dot, na2, nb2 float64
	for i := range a {
		va := float64(a[i])
		vb := float64(b[i])
		dot += va * vb
		na2 += va * va
		nb2 += vb * vb
	}
	if na2 == 0 || nb2 == 0 {
		return 0, fmt.Errorf("vector: cosine similarity with zero-magnitude vector")
	}
	return dot / (math.Sqrt(na2) * math.Sqrt(nb2)), nil
}

// L2Distance computes the Euclidean (L2) distance between two vectors. It
// returns an error if the vectors have different lengths.
func L2Distance(a, b []float32) (float64, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("vector: L2 distance dimension mismatch: %d vs %d", len(a), len(b))
	}
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum), nil
}

