//go:build !darwin && !windows

package socket

import "net"

// peerCredSupported is false here: no platform in this build tag (linux and the
// other unixes runny does not ship a control-plane peer read for) has a
// kernel-authenticated peer-identity read wired up. darwin reads SO_PEERCRED
// and windows impersonates the named-pipe client; everything else records the
// operator identity as unknown. newOperatorGate reads this to keep the
// operator-revocation gate unarmed here: arming it anyway would deny every RPC
// unconditionally (readPeer never resolves, and the gate fails closed on an
// unreadable identity by design — see docs/security.md's revocation section),
// locking a system daemon out of its own control socket rather than degrading
// gracefully to the socket-is-the-sole-gate baseline that security.md already
// documents as the primary tier.
const peerCredSupported = false

// readPeer has no portable peer read here; the peer identity is always
// unknown, and the conn passes through unchanged. The audit path records
// "unknown" (the same fail-open the darwin cred-read miss takes); the
// revocation gate stays unarmed (see peerCredSupported).
func readPeer(conn net.Conn) (net.Conn, string, bool, bool) {
	return conn, "", false, false
}
