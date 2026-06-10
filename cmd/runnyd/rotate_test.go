package main

import (
	"bytes"
	"os"
	"path/filepath"
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
