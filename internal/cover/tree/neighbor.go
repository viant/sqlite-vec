package tree

// Neighbor describes a candidate returned by a kNN search.
type Neighbor struct {
	Point    *Point
	Distance float32
}

// Neighbors implements heap.Interface sorted by descending distance (max-heap).
type Neighbors []Neighbor

func (h Neighbors) Len() int           { return len(h) }
func (h Neighbors) Less(i, j int) bool { return h[i].Distance > h[j].Distance }
func (h Neighbors) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func (h *Neighbors) Push(x interface{}) {
	*h = append(*h, x.(Neighbor))
}

func (h *Neighbors) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
