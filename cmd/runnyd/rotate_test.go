package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRotatingFileRotatesAtCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.log")
	r, err := openRotatingFile(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	line := bytes.Repeat([]byte("x"), 40)
	for range 4 { // 160 bytes total: rotation must trigger
		if _, err := r.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("no rotated generation: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 100 {
		t.Errorf("live log is %d bytes, past the cap", st.Size())
	}
}

func TestAcquireLockExcludesSecondInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.lock")
	first, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if second, err := acquireLock(path); err == nil {
		_ = second.Close()
		t.Fatal("second instance acquired the lock; it would sweep the first's live VMs")
	}
}

// A log file created by an older runny is 0600, and O_CREATE's mode applies only
// when the file is created — so an existing log stays unreadable to operators
// unless the mode is re-asserted on open.
func TestOpenRotatingFileReopensAnExistingLogsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not gate reads on windows; the inherited ACL entry covers this there")
	}
	path := filepath.Join(t.TempDir(), "runnyd.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rf, err := openRotatingFile(path, 1<<20)
	if err != nil {
		t.Fatalf("openRotatingFile: %v", err)
	}
	t.Cleanup(func() { rf.Close() })

	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o644 {
		t.Errorf("log mode = %04o, want 0644 — an existing log keeps its creation mode otherwise", got)
	}
}
