//go:build !darwin

package socket

// peerCredSupported is false here: no non-darwin platform this daemon ships
// on has a real peer-identity read wired up yet. Windows' AF_UNIX in
// particular has no SO_PEERCRED equivalent at all -- Microsoft's own AF_UNIX
// announcement lists ancillary credential passing as a deliberately
// unsupported feature, delegating socket authorization entirely to
// filesystem ACLs on the socket path instead. A weaker Windows read exists
// (a getpeerpid ioctl on the AF_UNIX socket, resolved to the process
// token's SID) and lands separately, armed only behind a startup self-probe
// proving the ioctl works on the running host. Until then newOperatorGate
// reads this to keep the operator-revocation gate unarmed here: arming it
// anyway would deny every RPC unconditionally (peerID never resolves, and
// the gate fails closed on an unreadable identity by design -- see
// docs/security.md's revocation section), locking a system daemon out of
// its own control socket rather than degrading gracefully to the
// socket-is-the-sole-gate baseline that security.md already documents as
// the primary tier.
const peerCredSupported = false

// readPeerID has no portable peer-credential read; every non-darwin daemon
// records the operator identity as unknown rather than failing the request
// (the same fail-open the darwin cred-read miss takes) for the
// injected_keys audit-stamping use. The operator-revocation gate (a
// stricter, fail-closed consumer of the same read) is kept unarmed here
// instead -- see peerCredSupported.
func readPeerID(fd uintptr) (string, bool) {
	return "", false
}

// privilegedPeerID is never consulted here today (the gate is unarmed --
// peerCredSupported), and no non-darwin platform has an always-authorized
// peer identity: Windows' SYSTEM and elevated Administrators bypass the
// home DACL through Full ACEs and SeTakeOwnershipPrivilege, not through a
// gate exemption. Returning false means an accidental future arming fails
// closed rather than minting a privileged principal by omission.
func privilegedPeerID(id string) bool { return false }
