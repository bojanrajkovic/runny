package main

import (
	"path/filepath"
	"testing"
)

// A second acquirer must be refused while the first holds the lock — that
// second runnyd would sweep the first's live VM disks and steal its socket.
func TestAcquireLockRefusesSecondAcquirer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runnyd.lock")
	f, err := acquireLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	second, err := acquireLock(path)
	if err == nil {
		second.Close()
		t.Fatal("second acquireLock succeeded; want refusal while the first holds the lock")
	}
}
