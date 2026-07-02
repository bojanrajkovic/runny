// Package traceid derives deterministic OTLP trace and span IDs from a
// cycle's own identity, with no OTEL import: internal/telemetry's trace
// consumer uses it to build spans, and callers like runnyctl can compute the
// same IDs from a cycle.json record without linking the OTEL SDK. A cycle's
// identity fully determines its trace: re-deriving from the same record
// always yields the same IDs, so re-emitting a retained cycle is idempotent.
package traceid

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// Trace derives a cycle's trace ID from the identity that uniquely names it.
func Trace(instancePrefix, slot, cycleID string, started time.Time) [16]byte {
	var out [16]byte
	copy(out[:], digest(instancePrefix, slot, cycleID, started.UTC().Format(time.RFC3339Nano)))
	return out
}

// Span derives a span ID scoped to traceID. kind distinguishes the span's
// place in the tree ("cycle", "step", or "action") so root/step/action
// spans never collide even when step or action is empty; seq is the
// originating obs.Event's Seq, unique within the cycle.
func Span(traceID [16]byte, kind, step, action string, seq uint64) [8]byte {
	var out [8]byte
	copy(out[:], digest(string(traceID[:]), kind, step, action, fmt.Sprintf("%d", seq)))
	return out
}

// digest hashes parts, retrying with an appended counter on the
// (astronomically unlikely) all-zero prefix — an OTLP ID is invalid if it's
// all zero, and callers must never hand one back. Bounded at 256 tries (a
// cumulative ~2^-2048 event) rather than looping on n's byte wraparound: a
// pure function has no deadline to carry, so a bound too small to ever be
// real is this package's only tool for staying a bound rather than a spin.
func digest(parts ...string) []byte {
	for n := range 256 {
		h := sha256.New()
		for _, p := range parts {
			h.Write([]byte(p))
			h.Write([]byte{0})
		}
		h.Write([]byte{byte(n)})
		sum := h.Sum(nil)
		if !allZero(sum[:16]) {
			return sum
		}
	}
	panic("traceid: 256 consecutive SHA-256 collisions on an all-zero prefix — cryptographically unreachable, so this indicates a broken hash implementation")
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
