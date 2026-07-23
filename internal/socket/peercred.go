package socket

import (
	"context"
	"net"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
)

// peerAuth is the AuthInfo peerCreds hands back from ServerHandshake: the
// kernel-authenticated identity of the connecting peer (SO_PEERCRED on
// darwin), read server-side so it cannot be forged by a client-supplied
// value. SecurityLevel is deliberately NoSecurity — the channel is a
// plaintext unix socket, and claiming a higher level would tell gRPC a
// future per-RPC secret is safe to send in the clear.
type peerAuth struct {
	credentials.CommonAuthInfo
	// ID is the platform-native identity string, following
	// os/user.User.Uid's convention: a decimal uid on darwin, a SID string
	// on Windows. nil when the daemon could not read it (an unsupported
	// platform, or a cred-read miss) — distinct from a real privileged peer
	// ("0" on darwin: root, which bypasses the socket's 0600 mode).
	ID *string
	// Privileged marks the platform's always-authorized principal — root on
	// darwin, SYSTEM or an elevated Administrators member on Windows. It is
	// determined by readPeerID alongside ID, while the platform has the
	// peer's kernel credential or token in hand: an identity string alone
	// cannot answer Windows admin membership, so the verdict travels with
	// the identity instead of being re-derived from it. Meaningless when ID
	// is nil.
	Privileged bool
}

func (peerAuth) AuthType() string { return "peercred" }

// peerCreds is a server-only credentials.TransportCredentials: it reads the
// connecting peer's identity during ServerHandshake and otherwise does no
// handshaking at all, like insecure.NewCredentials — which it embeds to
// inherit ClientHandshake/OverrideServerName unchanged (every runny client
// dials with insecure.NewCredentials, never this type, so ClientHandshake is
// never actually exercised). Info and Clone are overridden rather than left
// to promote: unlike the client-only fields above, grpc-go's server path
// could plausibly clone or introspect server creds in a future version, and a
// promoted Clone would silently return a bare insecure value that has lost
// ServerHandshake entirely — a security-relevant regression with no error and
// no obvious test failure, worth the two extra methods to foreclose.
type peerCreds struct {
	credentials.TransportCredentials
}

func newPeerCreds() credentials.TransportCredentials {
	return peerCreds{TransportCredentials: insecure.NewCredentials()}
}

func (peerCreds) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	auth := peerAuth{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}
	if uc, ok := conn.(*net.UnixConn); ok {
		if sc, err := uc.SyscallConn(); err == nil {
			_ = sc.Control(func(fd uintptr) {
				if id, privileged, ok := readPeerID(fd); ok {
					auth.ID = &id
					auth.Privileged = privileged
				}
			})
		}
	}
	return conn, auth, nil
}

func (peerCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (c peerCreds) Clone() credentials.TransportCredentials { return c }

// peerID extracts the calling peer's kernel-authenticated identity and
// privileged verdict set by peerCreds during ServerHandshake. ok is false
// whenever the identity is not known — no peer in ctx, a foreign AuthInfo
// implementation, or a nil ID — never a client-controlled value.
func peerID(ctx context.Context) (id string, privileged, ok bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", false, false
	}
	auth, ok := p.AuthInfo.(peerAuth)
	if !ok || auth.ID == nil {
		return "", false, false
	}
	return *auth.ID, auth.Privileged, true
}
