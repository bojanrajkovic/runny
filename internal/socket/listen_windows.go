//go:build windows

package socket

import (
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// fileCreatePipeInstance is FILE_CREATE_PIPE_INSTANCE: the right to add another
// server instance to a pipe NAME that already exists. It is the bit files call
// FILE_APPEND_DATA, and on NPFS it is a member of FILE_GENERIC_WRITE — so ANY
// grant of generic write carries it, which is why the fix here is an explicit
// mask rather than GENERIC_ALL narrowed to GENERIC_READ|GENERIC_WRITE.
const fileCreatePipeInstance = 0x0004

// clientAccessMask is what a control-channel client is granted: the union of
// FILE_GENERIC_READ (0x120089) and FILE_GENERIC_WRITE (0x120116) with
// FILE_CREATE_PIPE_INSTANCE cleared. Read and write data, EA and attributes,
// READ_CONTROL (the client needs it to read the pipe's owner) and SYNCHRONIZE.
//
// Deliberately NOT coupled to runnyctl's dial access, which stays generic. A
// client opening the pipe with GENERIC_READ|GENERIC_WRITE is admitted by this
// descriptor, while a server-side attempt to add an instance under the same
// grant is refused: client opens and instance creation do not resolve the
// generic mask the same way. TestClientGrantWithholdsInstanceCreation pins the
// property that matters — that this mask never carries instance creation.
const clientAccessMask = 0x12019B

// systemPipeSDDLFormat is the system daemon pipe's security descriptor: owner
// BUILTIN\Administrators (O:BA), full access for the daemon's own principal,
// and the restricted client mask for Authenticated Users.
//
// The daemon's own ACE is not decoration. winio adds every instance after the
// first by opening the existing name for GENERIC_READ|GENERIC_WRITE, which
// expands to include FILE_CREATE_PIPE_INSTANCE — so a descriptor that grants
// the daemon nothing of its own would let it bind once and then fail to serve a
// second client. It names the running token's SID rather than Administrators
// because the service account is not required to be an administrator.
//
// Withholding that bit from AU is what closes the real squat. A pipe's security
// descriptor is per-NAME, fixed by the first instance, so an instance added by
// another process inherits this owner — measured on a live daemon, a rogue
// instance reported owner BUILTIN\Administrators and the client's owner check
// passed against it. The owner check cannot see instances; only the DACL can
// stop them existing.
//
// AU, not Everyone (WD): an unauthenticated logon has no business reaching the
// per-RPC operator-revocation gate, which remains the real authorization tier —
// this DACL is a coarse connect filter, not the whole story.
const systemPipeSDDLFormat = "O:BAD:(A;;FA;;;%s)(A;;0x%x;;;AU)"

// pipeBufferBytes sizes the pipe's kernel input/output buffers. A zero-buffer
// (winio's default) pipe makes every client WriteFile rendezvous with the
// server reader — the write does not complete until the server has read every
// byte. The handshake reads the client SID by impersonating the pipe, and that
// impersonation peeks only the client's first byte before pausing to read the
// token; a client mid-write would stall against the paused reader (a real
// deadlock, seen in TestReadPeerImpersonatesClientSID). A non-trivial buffer
// lets the client's handshake bytes land immediately, so no client write ever
// blocks on the server's read cadence. 64 KiB comfortably holds gRPC's HTTP/2
// preface and settings.
const pipeBufferBytes = 64 * 1024

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
	ln, err := winio.ListenPipe(path, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		InputBufferSize:    pipeBufferBytes,
		OutputBufferSize:   pipeBufferBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	return ln, nil
}

// pipeSDDL builds the pipe's connect security descriptor. Both branches name
// the running token's own SID: the system daemon grants itself full access and
// Authenticated Users the restricted client mask; a per-user daemon grants only
// itself, owner-only, the pipe-namespace analogue of darwin's 0600 socket.
func pipeSDDL(systemDaemon bool) (string, error) {
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("resolving current user SID for the pipe ACL: %w", err)
	}
	own := tu.User.Sid.String()
	if systemDaemon {
		return fmt.Sprintf(systemPipeSDDLFormat, own, clientAccessMask), nil
	}
	// O: is pinned, not left to the token default: "System objects: Default
	// owner for objects created by members of the Administrators group" makes
	// an elevated per-user daemon's pipe owned by Administrators, and the
	// client verifies the owner is the resolving user. Assert the property you
	// create.
	return "O:" + own + "D:(A;;GA;;;" + own + ")", nil
}
