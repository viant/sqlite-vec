package tree

// Point represents a vector in the cover tree.
type Point struct {
	index     int32
	Magnitude float32
	Vector    []float32
}

// HasValue reports whether the point has an associated value.
func (p *Point) HasValue() bool {
	return p != nil && p.index >= 0
}

// NewPoint constructs a point for the given vector.
func NewPoint(vector ...float32) *Point {
	return &Point{Vector: vector}
}
