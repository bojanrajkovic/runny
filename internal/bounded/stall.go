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

// tickerInterval is the stall poll interval: window/4, but never zero. For a
// window in [1ns, 3ns] the integer division floors to 0, and time.NewTicker(0)
// panics — in Watch's watcher goroutine, with no recover, crashing the daemon.
// The window>0 guard in Watch does not catch this (1ns is positive); this does.
func tickerInterval(window time.Duration) time.Duration {
	if iv := window / 4; iv > 0 {
		return iv
	}
	return window // window is positive (Watch guards window>0) but < 4ns
}

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
		t := time.NewTicker(tickerInterval(window))
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
