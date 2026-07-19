//go:build windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
)

// runSSH: windows has no exec-replace, so ssh runs as a child with the
// terminal passed through, and runnyctl exits with ssh's exit code. The
// os.Exit path removes the knownHosts temp file itself, because os.Exit
// skips the caller's deferred cleanup; the plain returns leave it to that
// defer (see execSSH).
func runSSH(sshPath string, argv []string, knownHosts string) error {
	// Ctrl+C is broadcast to the whole console process group; without this the
	// parent dies alongside ssh, skipping every cleanup path below. Ignore it
	// for the child's lifetime — ssh owns the terminal until it exits.
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	cmd := exec.Command(sshPath, argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if knownHosts != "" {
			_ = os.Remove(knownHosts)
		}
		os.Exit(ee.ExitCode())
	}
	return err
}
