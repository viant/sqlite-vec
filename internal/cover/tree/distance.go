package tree

import "github.com/viant/vec/search"

// DistanceFunction enumerates supported distance metrics for the cover tree.
type DistanceFunction string

const (
	DistanceFunctionCosine    DistanceFunction = "cosine"
	DistanceFunctionEuclidean DistanceFunction = "euclidean"
)

// DistanceFunc computes the distance between two points.
type DistanceFunc func(p1, p2 *Point) float32

// Function resolves the callable distance implementation.
func (d DistanceFunction) Function() DistanceFunc {
	switch d {
	case DistanceFunctionCosine:
		return CosineDistance
	case DistanceFunctionEuclidean:
		return EuclideanDistance
	default:
		return nil
	}
}

// CosineDistance returns the cosine distance (1 - cosine similarity).
func CosineDistance(p1, p2 *Point) float32 {
	v1 := search.Float32s(p1.Vector)
	m1 := p1.Magnitude
	if m1 == 0 {
		m1 = v1.Magnitude()
	}
	v2 := search.Float32s(p2.Vector)
	m2 := p2.Magnitude
	if m2 == 0 {
		m2 = v2.Magnitude()
	}
	return v1.CosineDistanceWithMagnitude(p2.Vector, m1, m2)
}

// EuclideanDistance returns the Euclidean distance between two points.
func EuclideanDistance(p1, p2 *Point) float32 {
	return search.Float32s(p1.Vector).EuclideanDistance(p2.Vector)
}
