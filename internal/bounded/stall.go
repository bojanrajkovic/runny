package bounded

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Stall is the second legitimate bound: a transfer whose duration cannot be
// known up front (image pulls range from seconds to hours) is bounded by
// progress instead of wall-clock — no bytes for `window` means stuck, per
// the image-economics learning: a slow pull is expected, a silent one is
// not. Feed it byte deltas; Watch cancels when they stop.
type Stall struct {
	mu   sync.Mutex
	last time.Time
}

func NewStall() *Stall { return &Stall{last: time.Now()} }

func (s *Stall) Feed(int64) {
	s.mu.Lock()
	s.last = time.Now()
	s.mu.Unlock()
}

// Watch cancels the returned context when no progress arrives for window.
// The watcher itself bounds the context from the moment Watch is called, so
// even a transfer that never produces its first byte (a server that accepts
// TCP and goes silent) fails within window.
func (s *Stall) Watch(ctx context.Context, window time.Duration) (Context, context.CancelFunc) {
	wctx, cancel := context.WithCancelCause(ctx)
	if window <= 0 {
		// A non-positive window is a misconfiguration: fail loudly now
		// rather than panic in the ticker or watch nothing.
		cancel(fmt.Errorf("stall window must be positive, got %v", window))
		return Context{ctx: wctx}, func() { cancel(nil) }
	}
	go func() {
		t := time.NewTicker(window / 4)
		defer t.Stop()
		for {
			select {
			case <-wctx.Done():
				return
			case <-t.C:
				s.mu.Lock()
				idle := time.Since(s.last)
				s.mu.Unlock()
				if idle > window {
					cancel(fmt.Errorf("transfer stalled: no progress for %v", window))
					return
				}
			}
		}
	}()
	return Context{ctx: wctx}, func() { cancel(nil) }
}
