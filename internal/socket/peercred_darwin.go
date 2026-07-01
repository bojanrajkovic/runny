//go:build darwin

package socket

import "golang.org/x/sys/unix"

// readPeerUID reads the connecting peer's kernel-authenticated uid via
// SO_PEERCRED (LOCAL_PEERCRED on darwin). Validated against a real unix
// socket by the multi-operator design spike (aclprobe): a fresh 0600 socket's
// connecting peer resolves correctly with no client cooperation required.
func readPeerUID(fd uintptr) (uint32, bool) {
	xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return 0, false
	}
	return xu.Uid, true
}
