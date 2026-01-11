package bamlutils

import "sync"

// Pool is a type-safe wrapper around sync.Pool.
// It provides generic Get and Put operations without requiring type assertions.
type Pool[T any] struct {
	pool sync.Pool
}

// NewPool creates a new Pool with the given constructor function.
// The constructor is called when the pool is empty and a new item is needed.
func NewPool[T any](new func() T) *Pool[T] {
	return &Pool[T]{
		pool: sync.Pool{
			New: func() any {
				return new()
			},
		},
	}
}

// Get retrieves an item from the pool or creates a new one if the pool is empty.
func (p *Pool[T]) Get() T {
	return p.pool.Get().(T)
}

// Put returns an item to the pool for reuse.
// The caller should ensure the item is reset to a clean state before putting it back.
func (p *Pool[T]) Put(x T) {
	p.pool.Put(x)
}
