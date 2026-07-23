//go:build windows

package socket

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// authenticatedUsersSDDL grants pipe connect to Authenticated Users. On the
// system daemon it is a coarse connect filter, deliberately NOT the sole
// authorization tier the way a unix socket's 0600 mode is on darwin: any
// authenticated principal can open the pipe, but the per-RPC
// operator-revocation gate (armed unconditionally on the system daemon — the
// handshake reads the client's kernel-established SID by impersonation, so
// there is no unreadable-identity failure to degrade around) fails every RPC
// closed for a principal absent from the home ACL. AU, not Everyone (WD): an
// unauthenticated / anonymous logon has no business even reaching the gate.
const authenticatedUsersSDDL = "D:(A;;GA;;;AU)"

// listen binds the control channel for a windows daemon: a named pipe. winio's
// ListenPipe creates each instance FILE_FLAG_OVERLAPPED and hands back an
// async net.Conn driven over an IOCP — the shape gRPC needs — and its concrete
// conn promotes Fd(), which the handshake type-asserts to reach the raw pipe
// HANDLE for the one ImpersonateNamedPipeClient at connect. Hand-rolling
// CreateNamedPipe + an overlapped-handle net.Conn adapter would reimplement
// exactly this.
//
// systemDaemon picks the pipe's connect DACL: the system daemon grants
// Authenticated Users (above); a per-user daemon grants only its own owner SID,
// so no other local user can open its pipe — the owner-only analogue of
// darwin's 0600 per-user socket.
func listen(path string, systemDaemon bool) (net.Listener, error) {
	sddl, err := pipeSDDL(systemDaemon)
	if err != nil {
		return nil, err
	}
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: sddl})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	return ln, nil
}

// pipeSDDL builds the pipe's connect security descriptor: Authenticated Users
// for the system daemon, the resolving user's own SID (owner-only) for a
// per-user daemon.
func pipeSDDL(systemDaemon bool) (string, error) {
	if systemDaemon {
		return authenticatedUsersSDDL, nil
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolving current user SID for the per-user pipe ACL: %w", err)
	}
	return "D:(A;;GA;;;" + tu.User.Sid.String() + ")", nil
}
