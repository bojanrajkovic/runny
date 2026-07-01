//go:build !darwin

package socket

// readPeerUID has no portable peer-credential read; every non-darwin daemon
// records the operator uid as unknown rather than failing the request (the
// same fail-open the darwin cred-read miss takes).
func readPeerUID(fd uintptr) (uint32, bool) {
	return 0, false
}
