package socket

import "sync"

// fanoutRegistry is a mutex-guarded, opaque-int-keyed set of values: register
// adds one and returns a deregister func to remove it later (typically
// deferred), forEach visits a snapshot-safe copy of the current set. Shared
// by the watch-status fan-out (values are notify channels) and the operator
// revocation gate's live-stream registry (values are stream cancel funcs) —
// both are "N goroutines register interest, something else occasionally
// walks the set," just over different value types.
type fanoutRegistry[V any] struct {
	mu     sync.Mutex
	nextID int
	items  map[int]V
}

func newFanoutRegistry[V any]() *fanoutRegistry[V] {
	return &fanoutRegistry[V]{items: map[int]V{}}
}

func (r *fanoutRegistry[V]) register(v V) (deregister func()) {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	r.items[id] = v
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.items, id)
		r.mu.Unlock()
	}
}

func (r *fanoutRegistry[V]) forEach(fn func(V)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.items {
		fn(v)
	}
}

// len reports the current registered count — test-only visibility into
// register/deregister bookkeeping.
func (r *fanoutRegistry[V]) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}
