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
