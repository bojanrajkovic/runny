//go:build windows

package main

import (
	"context"
	"fmt"
	"net"

	winio "github.com/Microsoft/go-winio"
	"github.com/bojanrajkovic/runny/internal/home"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// pipeDialAccess is GENERIC_READ|GENERIC_WRITE — the duplex access a gRPC
// client needs on the pipe.
//
// Deliberately left generic even though the daemon's DACL grants Authenticated
// Users an explicit mask that withholds FILE_CREATE_PIPE_INSTANCE. Measured on
// Windows: a client opening with GENERIC_READ|GENERIC_WRITE is admitted by that
// descriptor, while a server-side attempt to add a pipe instance under the same
// grant is denied. Client opens and instance creation do not resolve the
// generic mask the same way, so there is no coupling here to keep in step — and
// an older runnyctl talks to a newer daemon unchanged.
const pipeDialAccess = 0x80000000 | 0x40000000

// Trusted control-pipe owner SIDs — a secondary check, not the anti-squat
// anchor it was once described as.
//
// Pre-creation is already prevented a layer down: winio creates the first
// instance with NT disposition FILE_CREATE, so if the name already exists the
// daemon fails to bind rather than adopting a squatter's object. That closes
// the restart-window MITM without reference to owners.
//
// What the owner check cannot do is see INSTANCES. A pipe's security descriptor
// belongs to the name and is fixed by the first instance, so an instance added
// by another process inherits it — measured on a live daemon, a rogue instance
// reported owner BUILTIN\Administrators and this check passed against it. The
// DACL withholding FILE_CREATE_PIPE_INSTANCE is what stops that; this remains
// as cheap defence in depth against an owner that is somehow neither.
const (
	sidAdministrators = "S-1-5-32-544"
	sidLocalSystem    = "S-1-5-18"
)

// isTrustedPipeOwner reports whether the pipe owner SID is a principal an
// unprivileged squatter cannot impersonate as an object owner. Any other SID —
// including an arbitrary user SID — is untrusted.
func isTrustedPipeOwner(sidString string) bool {
	return sidString == sidAdministrators || sidString == sidLocalSystem
}

// pipeOwnerSID reads the owner SID of the kernel pipe object backing conn. The
// winio pipe conn promotes Fd(), which yields the raw pipe HANDLE the security
// query needs.
func pipeOwnerSID(conn net.Conn) (string, error) {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return "", fmt.Errorf("pipe conn does not expose Fd() — cannot read its owner")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return "", fmt.Errorf("reading pipe owner security info: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return "", fmt.Errorf("extracting pipe owner SID: %w", err)
	}
	if owner == nil {
		return "", fmt.Errorf("pipe has no owner SID")
	}
	return owner.String(), nil
}

// verifyPipeOwner fails closed unless the dialed pipe is owned by a trusted
// principal. The server already authenticates the client (SID by impersonation
// at the handshake); this is the client authenticating the server, closing the
// pipe-squat MITM window before any RPC or credential (debug-key SSH material)
// crosses the connection. A read failure is treated as untrusted: the owner
// read is reliable on a live dialed pipe, so a failure is anomalous and
// refusing is the correct posture for a security check.
func verifyPipeOwner(conn net.Conn) error {
	owner, err := pipeOwnerSID(conn)
	if err != nil {
		return fmt.Errorf("cannot verify runnyd control pipe owner, refusing to connect: %w", err)
	}
	if !isTrustedPipeOwner(owner) {
		return fmt.Errorf("runnyd control pipe is not owned by Administrators or SYSTEM (owner %s) — refusing to connect; the pipe may be squatted", owner)
	}
	return nil
}

// verifyPipeOwnerIsSelf is verifyPipeOwner's per-user counterpart: identity,
// not privilege, since a per-user daemon's pipe is owned by the resolving user
// and Administrators/SYSTEM would refuse a healthy one.
//
// The skip it replaces reasoned from the healthy pipe's owner-only DACL — but
// a DACL belongs to whoever creates the name first, so a squatter's says
// nothing about who created it.
func verifyPipeOwnerIsSelf(conn net.Conn) error {
	owner, err := pipeOwnerSID(conn)
	if err != nil {
		return fmt.Errorf("cannot verify runnyd control pipe owner, refusing to connect: %w", err)
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("cannot read this process's own identity to verify the control pipe owner, refusing to connect: %w", err)
	}
	if self := tu.User.Sid.String(); owner != self {
		return fmt.Errorf("per-user runnyd control pipe is owned by %s, not by this user (%s) - refusing to connect; the pipe may be squatted", owner, self)
	}
	return nil
}

// dial connects to the daemon's named pipe. The pipe is dialed at
// SECURITY_IDENTIFICATION (winio.PipeImpLevelIdentification): the daemon reads
// the client's SID by impersonating this connection at the handshake, and
// winio's default Anonymous level yields an unreadable anonymous token there
// ("cannot open an anonymous level security token"). Identification lets the
// server read identity and group membership but grants it no rights to act as
// the client — the minimum the operator-revocation read requires.
//
// After dialing the SYSTEM pipe, the client verifies its owner is Administrators
// or SYSTEM before handing the conn to gRPC — mutual authentication that refuses
// a squatted pipe before any RPC or credential is sent.
//
// passthrough:runnyd, not a target grpc must resolve: the real endpoint is the
// context dialer, and passthrough hands the client a single fixed address so no
// resolver ever parses the pipe path (which is not a host:port).
func dial(socketPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:runnyd",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			conn, err := winio.DialPipeAccessImpLevel(ctx, socketPath, pipeDialAccess, winio.PipeImpLevelIdentification)
			if err != nil {
				return nil, err
			}
			// Both pipes are owner-verified, by different rules: the system
			// pipe must be owned by Administrators/SYSTEM, the per-user one by
			// this user. Every pipe name here is derivable, so a squatter can
			// pre-create either.
			verify := verifyPipeOwnerIsSelf
			if socketPath == home.PipeName {
				verify = verifyPipeOwner
			}
			if err := verify(conn); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return conn, nil
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// socketPresent steers only connHint's wording. On windows the control channel
// is a named pipe in the kernel's pipe namespace, not a file: os.Stat would
// open it as a client (consuming a pipe instance and tripping a server-side
// handshake), so this reports false and connHint falls back to its
// daemon-not-reachable phrasing.
//
// ponytail: always-false loses the hung-vs-absent distinction on windows; wire
// WaitNamedPipe if that wording ever matters.
func socketPresent(string) bool { return false }
