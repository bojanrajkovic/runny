//go:build darwin

package socket

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// peerCredSupported is the per-platform arming check newOperatorGate runs:
// only a platform with a real kernel-authenticated peer-identity read can
// enforce the operator-revocation gate's fail-closed contract without
// locking every RPC out permanently. LOCAL_PEERCRED is documented, always-on
// kernel surface, so darwin's answer is unconditional; on Windows the check
// is a live self-probe of an undocumented ioctl (see peercred_windows.go),
// which is why this is a function value rather than a const — and a var so
// tests can pin both arming branches on any platform.
var peerCredSupported = func() bool { return true }

// readPeerID reads the connecting peer's kernel-authenticated uid via
// SO_PEERCRED (LOCAL_PEERCRED on darwin), formatted as the decimal string
// os/user.User.Uid uses on unix — the platform-native identity convention
// every consumer (ACL membership, audit stamps, user.LookupId) shares.
// privileged carries the platform's always-authorized-principal verdict
// alongside the identity: root (uid 0) bypasses the socket's 0600 mode by
// design and holds no ACE, so the revocation gate must never deny it.
// Validated against a real unix socket by the multi-operator design spike
// (aclprobe): a fresh 0600 socket's connecting peer resolves correctly with
// no client cooperation required.
func readPeerID(fd uintptr) (id string, privileged, ok bool) {
	xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return "", false, false
	}
	return strconv.FormatUint(uint64(xu.Uid), 10), xu.Uid == 0, true
}
