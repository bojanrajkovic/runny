//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockExclusive takes a non-blocking exclusive flock. flock is advisory, but
// both contenders are runnyd, and the kernel drops the lock when the process
// dies.
func lockExclusive(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
