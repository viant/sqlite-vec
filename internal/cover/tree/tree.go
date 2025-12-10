package tree

// This implementation is adapted from github.com/viant/gds/tree/cover.

import (
	"container/heap"
	"math"
	"sort"
	"sync"

	"github.com/viant/vec/search"
)

// Tree represents a cover tree for cosine/euclidean kNN queries.
type Tree[T any] struct {
	root             *Node
	base             float32
	distanceFuncName DistanceFunction
	distanceFunc     DistanceFunc
	values           values[T]
	indexMap         map[int32]*Point
	version          uint64
	boundStrategy    BoundStrategy
	mu               sync.RWMutex
}

// BoundStrategy selects which lower-bound radius to use when pruning.
type BoundStrategy int

const (
	// BoundPerNode uses cached per-node subtree radius (tighter pruning).
	BoundPerNode BoundStrategy = iota
	// BoundLevel uses a geometric bound derived from the node level.
	BoundLevel
)

// NewTree constructs a cover tree with the provided base and distance metric.
func NewTree[T any](base float32, distanceFn DistanceFunction) *Tree[T] {
	if base <= 1 {
		base = 1.3
	}
	fn := distanceFn.Function()
	if fn == nil {
		fn = DistanceFunctionCosine.Function()
		distanceFn = DistanceFunctionCosine
	}
	return &Tree[T]{
		base:             base,
		distanceFuncName: distanceFn,
		distanceFunc:     fn,
		values:           values[T]{},
		boundStrategy:    BoundPerNode,
	}
}

// SetBoundStrategy switches the pruning strategy at runtime.
func (t *Tree[T]) SetBoundStrategy(s BoundStrategy) { t.boundStrategy = s }

// Insert adds a new value/vector pair to the tree and returns its index.
func (t *Tree[T]) Insert(value T, point *Point) int32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	point.index = t.values.put(value)
	if t.indexMap == nil {
		t.indexMap = make(map[int32]*Point)
	}
	t.indexMap[point.index] = point
	if point.Magnitude == 0 && len(point.Vector) > 0 {
		point.Magnitude = search.Float32s(point.Vector).Magnitude()
	}
	if t.root == nil {
		node := NewNode(point, 0, t.base)
		t.root = &node
	} else {
		t.insert(t.root, point, 0)
	}
	t.version++
	return point.index
}

// FindPointByIndex returns the point for a stored index.
func (t *Tree[T]) FindPointByIndex(index int32) *Point {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if point, ok := t.indexMap[index]; ok {
		return point
	}
	return nil
}

// Value returns the stored value for the given point.
func (t *Tree[T]) Value(point *Point) T {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var zero T
	if point == nil || !point.HasValue() {
		return zero
	}
	return t.values.value(point.index)
}

// Values resolves the stored values for the provided points.
func (t *Tree[T]) Values(points []*Point) []T {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]T, 0, len(points))
	for _, point := range points {
		if point == nil || point.index < 0 {
			continue
		}
		result = append(result, t.values.value(point.index))
	}
	return result
}

func (t *Tree[T]) insert(node *Node, point *Point, level int32) {
	for {
		baseLevel := float32(math.Pow(float64(t.base), float64(level)))
		distance := t.distanceFunc(point, node.point)
		if distance < baseLevel {
			inserted := false
			for i := range node.children {
				child := &node.children[i]
				if t.distanceFunc(point, child.point) < baseLevel {
					node = child
					level--
					inserted = true
					break
				}
			}
			if !inserted {
				node.children = append(node.children, NewNode(point, level-1, t.base))
				return
			}
		} else {
			level++
			if level > node.level {
				newRoot := NewNode(point, level, t.base)
				newRoot.children = append(newRoot.children, *t.root)
				t.root = &newRoot
				return
			}
		}
	}
}

// KNearestNeighbors runs a depth-first kNN search.
func (t *Tree[T]) KNearestNeighbors(point *Point, k int) []*Neighbor {
	if t.boundStrategy == BoundPerNode {
		t.mu.Lock()
		defer t.mu.Unlock()
	} else {
		t.mu.RLock()
		defer t.mu.RUnlock()
	}
	if t.root == nil {
		return nil
	}
	h := &Neighbors{}
	heap.Init(h)
	t.kNearestNeighbors(t.root, point, k, h)
	result := make([]*Neighbor, h.Len())
	for i := len(result) - 1; i >= 0; i-- {
		n := heap.Pop(h).(Neighbor)
		result[i] = &n
	}
	return result
}

func (t *Tree[T]) kNearestNeighbors(node *Node, point *Point, k int, h *Neighbors) {
	dc := t.distanceFunc(point, node.point)
	if h.Len() < k {
		heap.Push(h, Neighbor{Point: node.point, Distance: dc})
	} else if k > 0 && dc < (*h)[0].Distance {
		heap.Pop(h)
		heap.Push(h, Neighbor{Point: node.point, Distance: dc})
	}
	if len(node.children) == 0 {
		return
	}
	type childDist struct {
		child *Node
		dist  float32
	}
	cds := make([]childDist, 0, len(node.children))
	for i := range node.children {
		child := &node.children[i]
		cds = append(cds, childDist{child: child, dist: t.distanceFunc(point, child.point)})
	}
	sort.Slice(cds, func(i, j int) bool { return cds[i].dist < cds[j].dist })
	for _, cd := range cds {
		var worst float32 = float32(math.MaxFloat32)
		if h.Len() == k && k > 0 {
			worst = (*h)[0].Distance
		}
		r := t.boundRadius(cd.child)
		if h.Len() == k && (cd.dist-r) >= worst {
			continue
		}
		t.kNearestNeighbors(cd.child, point, k, h)
	}
}

// KNearestNeighborsBestFirst performs a best-first search with a node priority queue.
func (t *Tree[T]) KNearestNeighborsBestFirst(point *Point, k int) []*Neighbor {
	if t.boundStrategy == BoundPerNode {
		t.mu.Lock()
		defer t.mu.Unlock()
	} else {
		t.mu.RLock()
		defer t.mu.RUnlock()
	}
	if t.root == nil {
		return nil
	}
	nh := &Neighbors{}
	heap.Init(nh)
	pq := &nodeQueue{}
	heap.Init(pq)
	rootDist := t.distanceFunc(point, t.root.point)
	rootLB := rootDist - t.boundRadius(t.root)
	heap.Push(pq, nodeItem{node: t.root, lb: rootLB, centerDist: rootDist})

	for pq.Len() > 0 {
		worst := float32(math.MaxFloat32)
		if nh.Len() == k && k > 0 {
			worst = (*nh)[0].Distance
		}
		top := heap.Pop(pq).(nodeItem)
		if nh.Len() == k && top.lb >= worst {
			break
		}
		dc := top.centerDist
		if nh.Len() < k {
			heap.Push(nh, Neighbor{Point: top.node.point, Distance: dc})
		} else if dc < (*nh)[0].Distance {
			heap.Pop(nh)
			heap.Push(nh, Neighbor{Point: top.node.point, Distance: dc})
		}
		for i := range top.node.children {
			child := &top.node.children[i]
			cd := t.distanceFunc(point, child.point)
			lb := cd - t.boundRadius(child)
			if nh.Len() == k && lb >= (*nh)[0].Distance {
				continue
			}
			heap.Push(pq, nodeItem{node: child, lb: lb, centerDist: cd})
		}
	}
	result := make([]*Neighbor, nh.Len())
	for i := len(result) - 1; i >= 0; i-- {
		n := heap.Pop(nh).(Neighbor)
		result[i] = &n
	}
	return result
}

func (t *Tree[T]) ensureRadius(n *Node) float32 {
	if n == nil {
		return 0
	}
	if n.radiusComputed == t.version {
		return n.radius
	}
	if len(n.children) == 0 {
		n.radius = 0
		n.radiusComputed = t.version
		return 0
	}
	maxR := float32(0)
	for i := range n.children {
		child := &n.children[i]
		cr := t.ensureRadius(child)
		d := t.distanceFunc(n.point, child.point) + cr
		if d > maxR {
			maxR = d
		}
	}
	n.radius = maxR
	n.radiusComputed = t.version
	return maxR
}

func (t *Tree[T]) nodeCoverRadius(n *Node) float32 { return t.ensureRadius(n) }

func (t *Tree[T]) levelCoverRadius(n *Node) float32 {
	if t.base <= 1 || n == nil {
		return float32(math.MaxFloat32)
	}
	return n.baseLevel * t.base / (t.base - 1)
}

func (t *Tree[T]) boundRadius(n *Node) float32 {
	if t.boundStrategy == BoundLevel {
		return t.levelCoverRadius(n)
	}
	return t.nodeCoverRadius(n)
}

type nodeItem struct {
	node       *Node
	lb         float32
	centerDist float32
}

type nodeQueue []nodeItem

func (q nodeQueue) Len() int            { return len(q) }
func (q nodeQueue) Less(i, j int) bool  { return q[i].lb < q[j].lb }
func (q nodeQueue) Swap(i, j int)       { q[i], q[j] = q[j], q[i] }
func (q *nodeQueue) Push(x interface{}) { *q = append(*q, x.(nodeItem)) }
func (q *nodeQueue) Pop() interface{} {
	old := *q
	n := len(old)
	x := old[n-1]
	*q = old[:n-1]
	return x
}
