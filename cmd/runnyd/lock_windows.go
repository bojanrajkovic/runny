//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive takes a non-blocking exclusive LockFileEx region over the
// whole file. Unlike flock it is mandatory, but the property that matters is
// the same: the OS drops the lock when the handle closes with the process.
func lockExclusive(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, ^uint32(0), ^uint32(0), new(windows.Overlapped))
}
