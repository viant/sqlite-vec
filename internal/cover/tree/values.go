package tree

import (
	"sync"
)

type values[T any] struct {
	data []T
	mu   sync.RWMutex
}

func (v *values[T]) put(value T) int32 {
	v.mu.Lock()
	defer v.mu.Unlock()
	index := len(v.data)
	v.data = append(v.data, value)
	return int32(index)
}

func (v *values[T]) value(index int32) T {
	v.mu.RLock()
	defer v.mu.RUnlock()
	var zero T
	if index < 0 || int(index) >= len(v.data) {
		return zero
	}
	return v.data[index]
}

func (v *values[T]) len() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.data)
}
