//go:build !windows

package home

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// SystemHomeDir is the fixed home of a non-root system daemon: its ENTIRE state
// (config, the App key, logs, images, vms, cycles, and the control socket) lives
// here, deliberately outside any user home so the service account needs no home
// directory of its own. The privileged installer creates it owned by the
// dedicated service account, with an ACL entry granting the operator account
// write on the DIRECTORY (to land the App key and reach the socket) and nothing
// beneath it — config edits are RPCs, and artifacts below are readable by mode
// (Dir.Ensure) rather than by an entry a revoke could not take back. The socket stays 0600
// plus an operator entry the daemon stamps from the operator set when it
// creates it; it is never world-accessible (opening it is full daemon control).
// A second entry, for the service account, does inherit — that is what lets the
// daemon read an operator-landed 0600 config or key. No flag selects
// it — its presence (and ownership, for the daemon) does, over the per-user
// ~/.runny.
const SystemHomeDir = "/Library/Application Support/runny"

// SocketPath is the control socket runnyd binds and clients dial: a unix
// domain socket inside the resolved home. On windows it is a named pipe with a
// fixed name (see home_windows.go), so this method is per-platform.
func (d Dir) SocketPath() string { return filepath.Join(string(d), socketName) }

// ownedByCurrentUser reports whether path exists and is owned by this
// process's euid — the ownership (not writability) signal ResolveServer keys
// on.
func ownedByCurrentUser(path string) bool {
	var st unix.Stat_t
	return unix.Stat(path, &st) == nil && st.Uid == uint32(os.Geteuid())
}
