//go:build !darwin && !windows

package socket

// peerCredSupported is false here: no remaining platform (linux and
// friends) has a real peer-identity read wired up — darwin reads
// LOCAL_PEERCRED and Windows self-probes its getpeerpid ioctl, each in its
// own file. newOperatorGate reads this to keep the operator-revocation gate
// unarmed here: arming it anyway would deny every RPC unconditionally
// (peerID never resolves, and the gate fails closed on an unreadable
// identity by design — see docs/security.md's revocation section), locking
// a system daemon out of its own control socket rather than degrading
// gracefully to the socket-is-the-sole-gate baseline that security.md
// already documents as the primary tier. A var so tests can pin both arming
// branches on any platform.
var peerCredSupported = func() bool { return false }

// readPeerID has no peer-credential read here; the operator identity is
// recorded as unknown rather than failing the request (the same fail-open
// the darwin cred-read miss takes) for the injected_keys audit-stamping
// use. The operator-revocation gate (a stricter, fail-closed consumer of
// the same read) is kept unarmed here instead — see peerCredSupported. The
// privileged verdict is likewise never minted by omission: an accidental
// future arming fails closed.
func readPeerID(fd uintptr) (id string, privileged, ok bool) {
	return "", false, false
}
