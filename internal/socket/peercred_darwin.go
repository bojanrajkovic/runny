//go:build darwin

package socket

import (
	"net"
	"strconv"

	"golang.org/x/sys/unix"
)

// peerCredSupported gates newOperatorGate: only a platform with a real
// kernel-authenticated peer-identity read can enforce the operator-
// revocation gate's fail-closed contract without locking every RPC out
// permanently. See peercred_other.go for the non-darwin story.
const peerCredSupported = true

// readPeer reads the connecting peer's kernel-authenticated uid via
// SO_PEERCRED (LOCAL_PEERCRED on darwin), formatted as the decimal string
// os/user.User.Uid uses on unix — the platform-native identity convention
// every consumer (ACL membership, audit stamps, user.LookupId) shares. It
// returns the conn unchanged (darwin reads the identity out of band, off the
// socket fd, with no handshake byte to consume), the uid, and whether that uid
// is root (0), which bypasses the socket's 0600 mode by design and holds no
// ACE — the always-authorized principal the gate must never deny. Validated
// against a real unix socket by the multi-operator design spike (aclprobe): a
// fresh 0600 socket's connecting peer resolves correctly with no client
// cooperation required.
func readPeer(conn net.Conn) (net.Conn, string, bool, bool) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return conn, "", false, false
	}
	sc, err := uc.SyscallConn()
	if err != nil {
		return conn, "", false, false
	}
	var id string
	var got bool
	_ = sc.Control(func(fd uintptr) {
		xu, err := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			return
		}
		id = strconv.FormatUint(uint64(xu.Uid), 10)
		got = true
	})
	if !got {
		return conn, "", false, false
	}
	return conn, id, id == "0", true
}
