//go:build darwin

package socket

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// peerCredSupported gates newOperatorGate: only a platform with a real
// kernel-authenticated peer-identity read can enforce the operator-
// revocation gate's fail-closed contract without locking every RPC out
// permanently. See peercred_other.go for the non-darwin story.
const peerCredSupported = true

// readPeerID reads the connecting peer's kernel-authenticated uid via
// SO_PEERCRED (LOCAL_PEERCRED on darwin), formatted as the decimal string
// os/user.User.Uid uses on unix — the platform-native identity convention
// every consumer (ACL membership, audit stamps, user.LookupId) shares.
// Validated against a real unix socket by the multi-operator design spike
// (aclprobe): a fresh 0600 socket's connecting peer resolves correctly with
// no client cooperation required.
func readPeerID(fd uintptr) (string, bool) {
	xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return "", false
	}
	return strconv.FormatUint(uint64(xu.Uid), 10), true
}

// privilegedPeerID reports whether id is this platform's always-authorized
// principal: root (uid 0) bypasses the socket's 0600 mode by design and
// holds no ACE, so the revocation gate must never deny it.
func privilegedPeerID(id string) bool { return id == "0" }
