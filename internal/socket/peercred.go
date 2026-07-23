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
// plaintext local control channel (a unix socket on darwin, a named pipe on
// windows), and claiming a higher level would tell gRPC a future per-RPC
// secret is safe to send in the clear.
type peerAuth struct {
	credentials.CommonAuthInfo
	// ID is the platform-native identity string, following
	// os/user.User.Uid's convention: a decimal uid on darwin, a SID string
	// on Windows. nil when the daemon could not read it (an unsupported
	// platform, or a cred-read miss) — distinct from a real privileged peer
	// ("0" on darwin: root, which bypasses the socket's 0600 mode).
	ID *string
	// Privileged marks the platform's always-authorized principal, read at
	// the same handshake as ID: darwin root (uid 0), or windows SYSTEM /
	// an elevated Administrators-group member. The revocation gate skips the
	// ACL read for a privileged peer — it bypasses the socket/pipe's own
	// access control by design and holds no operator ACE, so denying it would
	// lock the platform's superuser out of its own daemon.
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
	// readPeer is per-platform: it reads the connecting peer's
	// kernel-authenticated identity (SO_PEERCRED on darwin, named-pipe client
	// impersonation on windows) and returns the conn gRPC should keep serving
	// — unchanged on darwin, a byte-replaying wrapper on windows where the read
	// had to peek the client's first byte. A read miss (ok=false) leaves ID
	// nil: the gate then fails closed, the audit stamp records "unknown".
	outConn, id, privileged, ok := readPeer(conn)
	if ok {
		auth.ID = &id
		auth.Privileged = privileged
	}
	return outConn, auth, nil
}

func (peerCreds) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "peercred"}
}

func (c peerCreds) Clone() credentials.TransportCredentials { return c }

// peerID extracts the calling peer's kernel-authenticated identity and
// privileged flag set by peerCreds during ServerHandshake. ok is false
// whenever the identity is not known — no peer in ctx, a foreign AuthInfo
// implementation, or a nil ID — never a client-controlled value. privileged
// is meaningful only when ok is true.
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
