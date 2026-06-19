package home

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SharedSocketDir is the fixed location of a system daemon's control socket,
// deliberately OUTSIDE the 0700 home (which holds credentials end to end — the
// App key, secret-bearing diag logs). The privileged installer creates it owned
// by the dedicated service account with an inheriting ACL granting the operator
// account; the socket itself stays 0600 + that inherited ACL and is never
// world-accessible (opening it is full daemon control). There is no flag: a
// per-user agent cannot write this dir and falls back to the home socket, so the
// presence of the installer-created dir is what selects the system-daemon path.
const SharedSocketDir = "/Library/Application Support/runny"

const socketName = "runnyd.sock"

// ServerSocketPath is where runnyd binds its control socket: the shared system
// socket when SharedSocketDir exists and is writable by this process (the
// system-daemon deployment, run as the service account that owns the dir), else
// the per-user home socket. Keyed off the dir, never a flag.
func ServerSocketPath(d Dir) string { return serverSocketPath(SharedSocketDir, d) }

func serverSocketPath(sharedDir string, d Dir) string {
	// Access() (not just Stat) so a per-user agent on a host that ALSO has a
	// system install — a misconfiguration — doesn't try to bind a dir it cannot
	// write and crash-loop; it falls back to its own home socket. macOS access()
	// honors the dir's ACL, so the service account that owns the dir passes.
	if unix.Access(sharedDir, unix.W_OK) == nil {
		return filepath.Join(sharedDir, socketName)
	}
	return d.SocketPath()
}

// ClientSocketPath is where runnyctl and other clients connect: the shared
// system socket when it exists, else the per-user home socket. Prefer-shared
// mirrors the server's bind so an operator's CLI reaches a system daemon with
// no configuration.
func ClientSocketPath(d Dir) string { return clientSocketPath(SharedSocketDir, d) }

func clientSocketPath(sharedDir string, d Dir) string {
	// Existence, not a connect() probe: this selects WHICH socket to dial, and the
	// dial itself (plus connHint) already reports a dead one. Existence diverges
	// from liveness only when a stale shared socket would mask a live per-user
	// daemon — which needs both a system and a per-user daemon on one host, the
	// channel mix the deployment model discourages. A connect probe wouldn't fix
	// that, only shift it (a momentarily-slow system daemon would then falsely
	// fall back to the per-user socket). Revisit if the system-daemon path ever
	// makes dual deployment routine.
	p := filepath.Join(sharedDir, socketName)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	return d.SocketPath()
}
