package logring

import (
	"fmt"
	"testing"
	"time"
)

// Regression: Subscribe once did blocking sends into the 256-cap subscriber
// channel while holding r.mu. With a ring holding more entries than the
// channel buffer and a client-controlled replay above the cap, the 257th send
// blocked forever — and every slog call in the daemon wedged behind the held
// mutex. Subscribe must return promptly no matter how large the replay is.
func TestSubscribeLargeReplayDoesNotBlock(t *testing.T) {
	r := NewRing(4096)
	for i := range 1024 {
		r.add(Entry{Time: time.Now(), Level: "INFO", Message: fmt.Sprintf("line %d", i)})
	}

	type result struct {
		snap []Entry
	}
	done := make(chan result)
	go func() {
		snap, _, cancel := r.Subscribe(1024)
		cancel()
		done <- result{snap: snap}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Subscribe blocked with replay > channel buffer")
	}
	if len(got.snap) != 1024 {
		t.Fatalf("snapshot has %d entries, want 1024", len(got.snap))
	}
	if got.snap[0].Message != "line 0" || got.snap[1023].Message != "line 1023" {
		t.Fatalf("snapshot misordered: first=%q last=%q", got.snap[0].Message, got.snap[1023].Message)
	}

	// And the ring must still accept entries afterwards (the daemon-wide
	// failure mode was add() wedging on the held mutex).
	added := make(chan struct{})
	go func() {
		r.add(Entry{Message: "after"})
		close(added)
	}()
	select {
	case <-added:
	case <-time.After(2 * time.Second):
		t.Fatal("add blocked after large-replay Subscribe")
	}
}

// The snapshot is taken atomically with registration: entries added after
// Subscribe returns show up on the channel only — no gaps, no duplicates.
func TestSubscribeSnapshotThenFollow(t *testing.T) {
	r := NewRing(8)
	r.add(Entry{Message: "old"})

	snap, ch, cancel := r.Subscribe(8)
	defer cancel()
	if len(snap) != 1 || snap[0].Message != "old" {
		t.Fatalf("snapshot = %v, want [old]", snap)
	}

	r.add(Entry{Message: "new"})
	select {
	case e := <-ch:
		if e.Message != "new" {
			t.Fatalf("followed entry = %q, want new", e.Message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("entry added after Subscribe never arrived on the channel")
	}
	select {
	case e := <-ch:
		t.Fatalf("unexpected extra entry %q (replay must not also hit the channel)", e.Message)
	default:
	}
}

// Zero replay subscribes follow-only.
func TestSubscribeZeroReplay(t *testing.T) {
	r := NewRing(8)
	r.add(Entry{Message: "old"})
	snap, _, cancel := r.Subscribe(0)
	defer cancel()
	if len(snap) != 0 {
		t.Fatalf("snapshot = %v, want empty", snap)
	}
}
