package main

import (
	"fmt"
	"os"
)

// acquireLock takes the single-instance lock. A second runnyd against the
// same home would sweep the first's live VM disks and steal its socket; the
// lock (lockExclusive, per platform) dies with the process — no
// stale-lockfile recovery needed after a crash.
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lock file: %w", err)
	}
	if err := lockExclusive(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another runnyd already owns %s — a second daemon would sweep the first's live VMs: %w", path, err)
	}
	return f, nil
}
