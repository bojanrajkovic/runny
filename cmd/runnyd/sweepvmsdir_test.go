package main

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestSweepVMsDirMissingIsNoOp: a daemon's very first cold start (no vms/
// yet) must not be treated as a sweep failure.
func TestSweepVMsDirMissingIsNoOp(t *testing.T) {
	if err := sweepVMsDir(filepath.Join(t.TempDir(), "does-not-exist"), slog.Default()); err != nil {
		t.Fatalf("sweepVMsDir = %v, want nil for a missing vms dir", err)
	}
}

// TestSweepVMsDirBestEffortContinuesPastFailure: one slot that can't be
// removed (removeAll is faked to fail for it, standing in for a real
// backend's still-open file handle -- OS permission bits don't portably
// simulate this, Windows ACLs don't honor chmod the way POSIX does) must
// not stop the others from being swept -- a single wedged slot must not
// crash-loop the whole sweep.
func TestSweepVMsDirBestEffortContinuesPastFailure(t *testing.T) {
	vmsDir := t.TempDir()
	for _, name := range []string{"slot-a", "slot-b", "slot-c"} {
		if err := os.Mkdir(filepath.Join(vmsDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	orig := removeAll
	removeAll = func(path string) error {
		if filepath.Base(path) == "slot-b" {
			return errors.New("simulated: still held open")
		}
		return orig(path)
	}
	t.Cleanup(func() { removeAll = orig })

	if err := sweepVMsDir(vmsDir, slog.Default()); err != nil {
		t.Fatalf("sweepVMsDir = %v, want nil (best-effort: a slot that can't be removed is logged, not fatal)", err)
	}
	for _, name := range []string{"slot-a", "slot-c"} {
		if _, err := os.Stat(filepath.Join(vmsDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s still exists (stat err = %v); want it removed despite slot-b failing", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(vmsDir, "slot-b")); err != nil {
		t.Errorf("slot-b (the wedged entry) should still exist, stat err = %v", err)
	}
}
