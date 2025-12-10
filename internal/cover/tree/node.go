package tree

import "math"

// Node represents a cover-tree node.
type Node struct {
	level          int32
	baseLevel      float32
	point          *Point
	children       []Node
	radius         float32
	radiusComputed uint64
}

// NewNode constructs a node for the provided point and level.
func NewNode(point *Point, level int32, base float32) Node {
	return Node{
		level:     level,
		baseLevel: float32(math.Pow(float64(base), float64(level))),
		point:     point,
	}
}
