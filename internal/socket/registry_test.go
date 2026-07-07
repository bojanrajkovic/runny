package socket

// len reports the current registered count — test-only visibility into
// register/deregister bookkeeping.
func (r *fanoutRegistry[V]) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}
