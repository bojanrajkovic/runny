//go:build windows

package socket

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
)

// pipeConnectSDDL is the named pipe's security descriptor: a DACL granting
// GENERIC_ALL to Authenticated Users (AU) and nothing else. It is a coarse
// connect filter, deliberately NOT the sole authorization tier the way a unix
// socket's 0600 mode is on darwin: any authenticated principal can open the
// pipe, but the per-RPC operator-revocation gate (armed unconditionally on the
// system daemon here — the handshake reads the client's kernel-established SID
// by impersonation, so there is no unreadable-identity failure to degrade
// around) fails every RPC closed for a principal absent from the home ACL. AU,
// not Everyone (WD): an unauthenticated / anonymous logon has no business even
// reaching the gate.
const pipeConnectSDDL = "D:(A;;GA;;;AU)"

// listen binds the control channel for a windows daemon: a named pipe. winio's
// ListenPipe creates each instance FILE_FLAG_OVERLAPPED and hands back an
// async net.Conn driven over an IOCP — the shape gRPC needs — and its concrete
// conn promotes Fd(), which the handshake type-asserts to reach the raw pipe
// HANDLE for the one ImpersonateNamedPipeClient at connect. Hand-rolling
// CreateNamedPipe + an overlapped-handle net.Conn adapter would reimplement
// exactly this.
func listen(path string) (net.Listener, error) {
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeConnectSDDL})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	return ln, nil
}
