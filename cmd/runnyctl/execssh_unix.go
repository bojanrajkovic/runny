//go:build !windows

package main

import (
	"os"
	"syscall"
)

// runSSH replaces the current process with ssh: the operator's session IS the
// ssh session, with no wrapper left to exit through. The knownHosts temp file
// is deliberately not cleaned up here — on success no deferred cleanup ever
// runs, and the OS sweeps /tmp (see execSSH).
func runSSH(sshPath string, argv []string, _ string) error {
	return syscall.Exec(sshPath, argv, os.Environ())
}
