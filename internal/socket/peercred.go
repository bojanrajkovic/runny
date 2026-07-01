package socket

import (
	"context"
	"net"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// peerAuth is the AuthInfo peerCreds hands back from ServerHandshake: the
// kernel-authenticated uid of the connecting peer (SO_PEERCRED on darwin),
// read server-side so it cannot be forged by a client-supplied value.
// SecurityLevel is deliberately NoSecurity — the channel is a plaintext unix
// socket, and claiming a higher level would tell gRPC a future per-RPC secret
// is safe to send in the clear.
type peerAuth struct {
	credentials.CommonAuthInfo
	// UID is nil when the daemon could not read it (non-darwin, or a
	// cred-read miss) — distinct from a real peer uid 0 (root, which bypasses
	// the socket's 0600 mode).
	UID *uint32
}

func (peerAuth) AuthType() string { return "peercred" }

// peerCreds is a server-only credentials.TransportCredentials: it reads the
// connecting peer's uid during ServerHandshake and otherwise does no
// handshaking at all (like insecure.NewCredentials, it wraps the conn without
// exchanging bytes). ClientHandshake exists only to satisfy the interface —
// every runny client (runnyctl, the app) dials with insecure.NewCredentials,
// never this type.
type peerCreds struct{}

func newPeerCreds() credentials.TransportCredentials { return peerCreds{} }

func (peerCreds) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, peerAuth{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}, nil
}

func (peerCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	auth := peerAuth{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}
	if uc, ok := conn.(*net.UnixConn); ok {
		if sc, err := uc.SyscallConn(); err == nil {
			_ = sc.Control(func(fd uintptr) {
				if uid, ok := readPeerUID(fd); ok {
					auth.UID = &uid
				}
			})
		}
	}
	return conn, auth, nil
}

func (peerCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (peerCreds) Clone() credentials.TransportCredentials { return peerCreds{} }

func (peerCreds) OverrideServerName(string) error { return nil }

// peerUID extracts the calling peer's kernel-authenticated uid set by
// peerCreds during ServerHandshake. ok is false whenever it is not known —
// no peer in ctx, a foreign AuthInfo implementation, or a nil UID — never a
// client-controlled value.
func peerUID(ctx context.Context) (uint32, bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return 0, false
	}
	auth, ok := p.AuthInfo.(peerAuth)
	if !ok || auth.UID == nil {
		return 0, false
	}
	return *auth.UID, true
}
