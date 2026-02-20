package cover

// Minimal placeholders to ease future swap with a full cover tree.
// The full implementation can be copied from viant/gds/tree/cover when needed.

// Point represents an embedding vector and its cached magnitude.
type Point struct {
	Vector    []float32
	Magnitude float32
	index     int32
}

// Node is a minimal tree node placeholder.
type Node struct {
	point    *Point
	level    int32
	children []*Node
}

// DistanceFunction names supported distance metrics.
type DistanceFunction string

const (
	Cosine DistanceFunction = "cosine"
	L2     DistanceFunction = "l2"
)

// Tree is a placeholder that may later be replaced by a true cover tree.
// For now it only stores points, and callers can build a higher-level index.
type Tree[T any] struct {
	base float32
	root *Node
}
