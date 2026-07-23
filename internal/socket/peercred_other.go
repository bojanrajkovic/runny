//go:build !darwin

package socket

// peerCredSupported is false here: no non-darwin platform this daemon ships
// on has a real peer-identity read wired up. Windows' AF_UNIX in particular
// has no SO_PEERCRED equivalent at all -- Microsoft's own AF_UNIX
// announcement lists ancillary credential passing as a deliberately
// unsupported feature, delegating socket authorization entirely to
// filesystem ACLs on the socket path instead. newOperatorGate reads this to
// keep the operator-revocation gate unarmed here: arming it anyway would
// deny every RPC unconditionally (peerUID never resolves, and the gate
// fails closed on an unreadable uid by design -- see docs/security.md's
// revocation section), locking a system daemon out of its own control
// socket rather than degrading gracefully to the socket-is-the-sole-gate
// baseline that security.md already documents as the primary tier. A real
// per-connection identity on Windows needs its own transport (a named pipe,
// impersonated to read the connecting token's SID) -- tracked separately,
// not attempted here.
const peerCredSupported = false

// readPeerUID has no portable peer-credential read; every non-darwin daemon
// records the operator uid as unknown rather than failing the request (the
// same fail-open the darwin cred-read miss takes) for the injected_keys
// audit-stamping use. The operator-revocation gate (a stricter, fail-closed
// consumer of the same read) is kept unarmed here instead -- see
// peerCredSupported.
func readPeerUID(fd uintptr) (uint32, bool) {
	return 0, false
}
