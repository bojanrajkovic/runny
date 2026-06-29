package bounded

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestWithTimeoutCarriesDeadline(t *testing.T) {
	ctx, cancel := WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("WithTimeout produced a context with no deadline")
	}
}

func TestWithDeadlineExpires(t *testing.T) {
	ctx, cancel := WithDeadline(t.Context(), time.Now().Add(20*time.Millisecond))
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline never fired")
	}
}

func TestStallWatch(t *testing.T) {
	s := NewStall()
	ctx, cancel := s.Watch(t.Context(), 200*time.Millisecond)
	defer cancel()
	// Feed for a while — must stay alive past the window.
	for range 3 {
		time.Sleep(80 * time.Millisecond)
		s.Feed(1)
	}
	select {
	case <-ctx.Done():
		t.Fatal("stall fired despite progress")
	default:
	}
	// Stop feeding — must fire.
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stall did not fire after progress stopped")
	}
	if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "stalled") {
		t.Errorf("cause = %v", cause)
	}
}

// A non-positive window must fail loudly, not panic in the ticker.
func TestStallWatchRejectsNonPositiveWindow(t *testing.T) {
	s := NewStall()
	ctx, cancel := s.Watch(t.Context(), 0)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("zero window did not fail the context")
	}
	if cause := context.Cause(ctx); cause == nil || !strings.Contains(cause.Error(), "positive") {
		t.Errorf("cause = %v", cause)
	}
}

// A stall watch with no progress at all must fire — the bound covers the
// first byte, not just stalls mid-transfer.
func TestStallWatchFiresWithoutFirstByte(t *testing.T) {
	s := NewStall()
	// Backdate so the test doesn't wait a real window.
	s.mu.Lock()
	s.last = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	ctx, cancel := s.Watch(t.Context(), 100*time.Millisecond)
	defer cancel()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stall did not fire when no byte ever arrived")
	}
}

func TestTickerInterval(t *testing.T) {
	// window/4 floors to 0 for window in [1ns, 3ns]; time.NewTicker(0) panics in
	// Watch's goroutine with no recover. The interval must never be < 1ns.
	for _, w := range []time.Duration{1, 2, 3} {
		if got := tickerInterval(w * time.Nanosecond); got <= 0 {
			t.Errorf("tickerInterval(%dns) = %v, want > 0 (NewTicker panics on 0)", w, got)
		}
	}
	// Normal windows still poll at a quarter.
	if got := tickerInterval(4 * time.Second); got != time.Second {
		t.Errorf("tickerInterval(4s) = %v, want 1s", got)
	}
}
