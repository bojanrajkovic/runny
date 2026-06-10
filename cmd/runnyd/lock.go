package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// acquireLock takes the single-instance lock. A second runnyd against the
// same home would sweep the first's live VM disks and steal its socket;
// flock is advisory, but both contenders are runnyd, and the lock dies with
// the process — no stale-lockfile recovery needed after a crash.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another runnyd already owns %s — a second daemon would sweep the first's live VMs: %w", path, err)
	}
	return f, nil
}
