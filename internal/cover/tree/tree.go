package tree

// This implementation is adapted from github.com/viant/gds/tree/cover.

import (
	"container/heap"
	"encoding/binary"
	"io"
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

// DistanceFunction reports the configured distance metric.
func (t *Tree[T]) DistanceFunction() DistanceFunction {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.distanceFuncName
}

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
		node := Node{level: 0, baseLevel: 1, point: point}
		t.root = &node
	} else {
		t.insert(t.root, point, 0, 1)
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

// ForEach visits each unique value/point pair.
func (t *Tree[T]) ForEach(fn func(value T, point *Point)) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.root == nil {
		return
	}
	visited := make(map[int32]struct{})
	var walk func(*Node)
	walk = func(n *Node) {
		if n == nil {
			return
		}
		if _, ok := visited[n.point.index]; ok {
			return
		}
		visited[n.point.index] = struct{}{}
		fn(t.values.value(n.point.index), n.point)
		for i := range n.children {
			walk(&n.children[i])
		}
	}
	walk(t.root)
}

func (t *Tree[T]) insert(node *Node, point *Point, level int32, baseLevel float32) {
	if baseLevel <= 0 {
		baseLevel = float32(math.Pow(float64(t.base), float64(level)))
	}
	for {
		distance := t.distanceFunc(point, node.point)
		if distance < baseLevel {
			inserted := false
			nextLevel := baseLevel / t.base
			for i := range node.children {
				child := &node.children[i]
				if t.distanceFunc(point, child.point) < baseLevel {
					node = child
					level--
					baseLevel = nextLevel
					inserted = true
					break
				}
			}
			if !inserted {
				childLevel := level - 1
				childBase := nextLevel
				if childBase <= 0 {
					childBase = float32(math.Pow(float64(t.base), float64(childLevel)))
				}
				node.children = append(node.children, Node{level: childLevel, baseLevel: childBase, point: point})
				return
			}
		} else {
			level++
			baseLevel *= t.base
			if level > node.level {
				newRoot := Node{level: level, baseLevel: baseLevel, point: point}
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

// EncodeBinary writes the tree (structure + values) using the provided encoder.
func (t *Tree[T]) EncodeBinary(w io.Writer, encodeValue func(io.Writer, T) error) error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if err := binary.Write(w, binary.LittleEndian, t.base); err != nil {
		return err
	}
	if err := writeString(w, string(t.distanceFuncName)); err != nil {
		return err
	}
	hasRoot := byte(0)
	if t.root != nil {
		hasRoot = 1
	}
	if err := binary.Write(w, binary.LittleEndian, hasRoot); err != nil {
		return err
	}
	if hasRoot == 0 {
		return nil
	}
	return t.encodeNode(w, t.root, encodeValue)
}

// DecodeBinary reconstructs the tree from the binary stream.
func (t *Tree[T]) DecodeBinary(r io.Reader, decodeValue func(io.Reader) (T, error)) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	var base float32
	if err := binary.Read(r, binary.LittleEndian, &base); err != nil {
		return err
	}
	name, err := readString(r)
	if err != nil {
		return err
	}
	t.base = base
	t.distanceFuncName = DistanceFunction(name)
	if fn := t.distanceFuncName.Function(); fn != nil {
		t.distanceFunc = fn
	} else {
		t.distanceFunc = DistanceFunctionCosine.Function()
		t.distanceFuncName = DistanceFunctionCosine
	}

	var hasRoot byte
	if err := binary.Read(r, binary.LittleEndian, &hasRoot); err != nil {
		return err
	}
	if hasRoot == 0 {
		t.root = nil
		t.indexMap = nil
		t.values = values[T]{}
		t.version++
		return nil
	}

	t.values = values[T]{}
	t.indexMap = make(map[int32]*Point)
	root, err := t.decodeNode(r, decodeValue)
	if err != nil {
		return err
	}
	t.root = root
	t.version++
	return nil
}

func (t *Tree[T]) encodeNode(w io.Writer, node *Node, encodeValue func(io.Writer, T) error) error {
	if err := binary.Write(w, binary.LittleEndian, node.level); err != nil {
		return err
	}
	if err := binary.Write(w, binary.LittleEndian, node.point.Magnitude); err != nil {
		return err
	}
	if err := writeFloat32Slice(w, node.point.Vector); err != nil {
		return err
	}
	value := t.values.value(node.point.index)
	if err := encodeValue(w, value); err != nil {
		return err
	}
	childCount := uint32(len(node.children))
	if err := binary.Write(w, binary.LittleEndian, childCount); err != nil {
		return err
	}
	for idx := range node.children {
		if err := t.encodeNode(w, &node.children[idx], encodeValue); err != nil {
			return err
		}
	}
	return nil
}

func (t *Tree[T]) decodeNode(r io.Reader, decodeValue func(io.Reader) (T, error)) (*Node, error) {
	var level int32
	if err := binary.Read(r, binary.LittleEndian, &level); err != nil {
		return nil, err
	}
	var magnitude float32
	if err := binary.Read(r, binary.LittleEndian, &magnitude); err != nil {
		return nil, err
	}
	vec, err := readFloat32Slice(r)
	if err != nil {
		return nil, err
	}
	val, err := decodeValue(r)
	if err != nil {
		return nil, err
	}
	index := t.values.put(val)
	point := &Point{
		index:     index,
		Magnitude: magnitude,
		Vector:    vec,
	}
	if t.indexMap == nil {
		t.indexMap = make(map[int32]*Point)
	}
	t.indexMap[index] = point

	var childCount uint32
	if err := binary.Read(r, binary.LittleEndian, &childCount); err != nil {
		return nil, err
	}
	children := make([]Node, childCount)
	for i := uint32(0); i < childCount; i++ {
		child, err := t.decodeNode(r, decodeValue)
		if err != nil {
			return nil, err
		}
		children[i] = *child
	}
	node := NewNode(point, level, t.base)
	node.children = children
	return &node, nil
}

func writeString(w io.Writer, value string) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(value))); err != nil {
		return err
	}
	if len(value) == 0 {
		return nil
	}
	_, err := io.WriteString(w, value)
	return err
}

func readString(r io.Reader) (string, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return "", err
	}
	if n == 0 {
		return "", nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func writeFloat32Slice(w io.Writer, vec []float32) error {
	if err := binary.Write(w, binary.LittleEndian, uint32(len(vec))); err != nil {
		return err
	}
	for _, v := range vec {
		if err := binary.Write(w, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return nil
}

func readFloat32Slice(r io.Reader) ([]float32, error) {
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, err
	}
	vec := make([]float32, n)
	for i := range vec {
		if err := binary.Read(r, binary.LittleEndian, &vec[i]); err != nil {
			return nil, err
		}
	}
	return vec, nil
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
