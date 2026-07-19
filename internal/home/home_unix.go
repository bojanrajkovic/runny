//go:build !windows

package home

import (
	"os"

	"golang.org/x/sys/unix"
)

// SystemHomeDir is the fixed home of a non-root system daemon: its ENTIRE state
// (config, the App key, logs, images, vms, cycles, and the control socket) lives
// here, deliberately outside any user home so the service account needs no home
// directory of its own. The privileged installer creates it owned by the
// dedicated service account with an inheriting ACL granting the operator account
// directory write (to land the App key and atomically edit config), artifact
// reads, and socket access; the socket stays 0600 + that inherited ACL and is
// never world-accessible (opening it is full daemon control). No flag selects
// it — its presence (and ownership, for the daemon) does, over the per-user
// ~/.runny.
const SystemHomeDir = "/Library/Application Support/runny"

// ownedByCurrentUser reports whether path exists and is owned by this
// process's euid — the ownership (not writability) signal ResolveServer keys
// on.
func ownedByCurrentUser(path string) bool {
	var st unix.Stat_t
	return unix.Stat(path, &st) == nil && st.Uid == uint32(os.Geteuid())
}
