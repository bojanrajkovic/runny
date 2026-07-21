package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func newTestEntry(t *testing.T) (*Entry, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return newEntry(slog.New(h)), &buf
}

func TestEntryFieldsFlowThrough(t *testing.T) {
	e, buf := newTestEntry(t)

	e.WithField("a", 1).WithFields(Fields{"b": 2}).WithError(errBoom).Info("hello")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	for k, want := range map[string]float64{"a": 1, "b": 2} {
		if got[k] != want {
			t.Errorf("field %q = %v, want %v", k, got[k], want)
		}
	}
	if got["error"] != errBoom.Error() {
		t.Errorf("field %q = %v, want %v", "error", got["error"], errBoom.Error())
	}
	if got[slog.MessageKey] != "hello" {
		t.Errorf("message = %v, want %q", got[slog.MessageKey], "hello")
	}
}

func TestEntryWithFieldDoesNotMutateOriginal(t *testing.T) {
	e, _ := newTestEntry(t)
	child := e.WithField("a", 1)
	if len(e.Data) != 0 {
		t.Fatalf("WithField mutated the receiver's Data: %v", e.Data)
	}
	if child.Data["a"] != 1 {
		t.Fatalf("child.Data[a] = %v, want 1", child.Data["a"])
	}
}

func TestEntryLevels(t *testing.T) {
	e, buf := newTestEntry(t)

	levels := map[string]func(){
		"debug": func() { e.Debug("d") },
		"info":  func() { e.Info("i") },
		"warn":  func() { e.Warn("w") },
	}
	for name, log := range levels {
		buf.Reset()
		log()
		if buf.Len() == 0 {
			t.Errorf("%s: expected a log line, got none", name)
		}
	}

	buf.Reset()
	e.Errorf("boom %d", 42)
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal log line: %v", err)
	}
	if got[slog.MessageKey] != "boom 42" {
		t.Errorf("message = %v, want %q", got[slog.MessageKey], "boom 42")
	}
}

func TestTraceIsBelowDebug(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	e := newEntry(slog.New(h))

	e.Trace("should not appear")
	if buf.Len() != 0 {
		t.Fatalf("Trace emitted at a Debug-level handler: %s", buf.String())
	}

	e.Debug("should appear")
	if buf.Len() == 0 {
		t.Fatal("Debug did not emit at a Debug-level handler")
	}
}

func TestDisabledLevelEmitsNothing(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	e := newEntry(slog.New(h))

	e.Debugf("disabled at Warn level")
	if buf.Len() != 0 {
		t.Fatalf("Debugf emitted at a Warn-level handler: %s", buf.String())
	}

	e.Warn("should appear")
	if buf.Len() == 0 {
		t.Fatal("Warn did not emit at a Warn-level handler")
	}
}

func TestGAndUpdateContext(t *testing.T) {
	ctx := context.Background()
	if G(ctx) != L {
		t.Fatalf("G(ctx) on a bare context should return the default entry L")
	}

	withField := L.WithField("scoped", true)
	ctx = context.WithValue(ctx, entryContextKey, withField)
	if G(ctx) != withField {
		t.Fatalf("G(ctx) did not return the stored entry")
	}

	ctx2 := UpdateContext(ctx)
	if G(ctx2) != withField {
		t.Fatalf("UpdateContext should re-store the current entry unchanged")
	}
}

var errBoom = errTest("boom")

type errTest string

func (e errTest) Error() string { return string(e) }
