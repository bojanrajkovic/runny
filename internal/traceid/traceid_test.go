package traceid

import (
	"testing"
	"time"
)

var started = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func TestTraceDeterministic(t *testing.T) {
	a := Trace("host-abcd1234", "pool-0", "a1b2c3d4", started)
	b := Trace("host-abcd1234", "pool-0", "a1b2c3d4", started)
	if a != b {
		t.Fatalf("Trace not deterministic: %x != %x", a, b)
	}
	if a == ([16]byte{}) {
		t.Fatal("Trace returned an all-zero (invalid) ID")
	}
}

func TestTraceDiscriminatesInputs(t *testing.T) {
	base := Trace("host-abcd1234", "pool-0", "a1b2c3d4", started)
	cases := map[string][16]byte{
		"instancePrefix": Trace("host-different", "pool-0", "a1b2c3d4", started),
		"slot":           Trace("host-abcd1234", "pool-1", "a1b2c3d4", started),
		"cycleID":        Trace("host-abcd1234", "pool-0", "deadbeef", started),
		"started":        Trace("host-abcd1234", "pool-0", "a1b2c3d4", started.Add(time.Second)),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("changing %s did not change the derived trace ID", name)
		}
	}
}

func TestSpanDeterministic(t *testing.T) {
	tid := Trace("host-abcd1234", "pool-0", "a1b2c3d4", started)
	a := Span(tid, "step", "BOOT", "", 3)
	b := Span(tid, "step", "BOOT", "", 3)
	if a != b {
		t.Fatalf("Span not deterministic: %x != %x", a, b)
	}
	if a == ([8]byte{}) {
		t.Fatal("Span returned an all-zero (invalid) ID")
	}
}

func TestSpanDiscriminatesInputs(t *testing.T) {
	tid := Trace("host-abcd1234", "pool-0", "a1b2c3d4", started)
	base := Span(tid, "step", "BOOT", "", 3)
	cases := map[string][8]byte{
		"kind":   Span(tid, "action", "BOOT", "", 3),
		"step":   Span(tid, "step", "CLONE", "", 3),
		"action": Span(tid, "step", "BOOT", "dial", 3),
		"seq":    Span(tid, "step", "BOOT", "", 4),
	}
	for name, got := range cases {
		if got == base {
			t.Errorf("changing %s did not change the derived span ID", name)
		}
	}
}

func TestSpanScopedToTrace(t *testing.T) {
	tidA := Trace("host-a", "pool-0", "a1b2c3d4", started)
	tidB := Trace("host-b", "pool-0", "a1b2c3d4", started)
	if Span(tidA, "step", "BOOT", "", 1) == Span(tidB, "step", "BOOT", "", 1) {
		t.Fatal("Span did not incorporate the trace ID: same span ID across different traces")
	}
}
