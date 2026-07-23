package socket

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// shortSocketDir returns a temp dir shorter than t.TempDir(), which embeds
// the full test name and overflows sun_path's ~104-byte limit for a unix
// socket bind.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestPeerCredsCarriesIdentityOverRealSocket proves the platform chain end to end:
// a real unix socket (bufconn has no fd, so SyscallConn().Control has nothing
// to read), the server installed with peerCreds, and an insecure client (the
// one every runny client actually is) still completing the handshake and
// landing the caller's uid in the RPC context.
func TestPeerCredsCarriesIdentityOverRealSocket(t *testing.T) {
	sock := filepath.Join(shortSocketDir(t), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	seen := make(chan peer.Peer, 1)
	captureUID := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if p, ok := peer.FromContext(ctx); ok {
			seen <- *p
		}
		return handler(ctx, req)
	}
	g := grpc.NewServer(grpc.Creds(newPeerCreds()), grpc.UnaryInterceptor(captureUID))
	srv := newTestServer(testSlots("mac-1"), nil, nil, nil)
	runnyv1.RegisterRunnyServiceServer(g, srv)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient(
		"unix:"+sock,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := runnyv1.NewRunnyServiceClient(conn)

	// Bound the RPC so a wedged handshake fails fast with a clear error rather
	// than blocking the suite until bazel's SIGKILL.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := client.GetStatus(ctx, &runnyv1.GetStatusRequest{}); err != nil {
		t.Fatalf("insecure client could not complete an RPC against a peerCreds server: %v", err)
	}

	var p peer.Peer
	select {
	case p = <-seen:
	default:
		t.Fatal("interceptor never observed a peer from context")
	}
	auth, ok := p.AuthInfo.(peerAuth)
	if !ok {
		t.Fatalf("AuthInfo is %T, want peerAuth", p.AuthInfo)
	}
	if auth.GetCommonAuthInfo().SecurityLevel.String() == "" {
		t.Error("SecurityLevel unset")
	}

	// The stub always reports "unknown" on non-darwin; darwin must resolve
	// the real connecting uid (this test process's own), formatted as
	// os/user.User.Uid's decimal string.
	if runtime.GOOS != "darwin" {
		if auth.ID != nil {
			t.Errorf("non-darwin stub must report unknown, got id %q", *auth.ID)
		}
		return
	}
	if auth.ID == nil {
		t.Fatal("darwin must read the peer identity, got unknown")
	}
	if want := strconv.Itoa(os.Getuid()); *auth.ID != want {
		t.Errorf("id = %q, want this process's uid %q", *auth.ID, want)
	}
	// The test process runs non-root under bazel, so the peer must read
	// non-privileged — the flag tracks the real handshake, not a hardcode.
	if os.Getuid() != 0 && auth.Privileged {
		t.Error("a non-root peer must not be marked privileged")
	}
}

func TestPeerID(t *testing.T) {
	id, _, ok := peerID(t.Context())
	if ok {
		t.Errorf("no peer in context: expected unknown, got id %q", id)
	}

	// Both identity shapes pass through verbatim: a darwin decimal uid and a
	// Windows SID are opaque strings to the transport layer.
	for _, want := range []string{"42", "S-1-5-21-1111111111-2222222222-3333333333-1001"} {
		w := want
		ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{ID: &w}})
		if id, priv, ok := peerID(ctx); !ok || id != want || priv {
			t.Errorf("peerID = (%q, %v, %v), want (%q, false, true)", id, priv, ok, want)
		}
	}

	// The privileged flag rides through alongside the identity.
	pid := "0"
	ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{ID: &pid, Privileged: true}})
	if id, priv, ok := peerID(ctx); !ok || id != "0" || !priv {
		t.Errorf("peerID = (%q, %v, %v), want (%q, true, true)", id, priv, ok, "0")
	}

	ctx = peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{ID: nil}})
	if _, _, ok := peerID(ctx); ok {
		t.Error("nil ID must report unknown")
	}

	ctx = peer.NewContext(t.Context(), &peer.Peer{AuthInfo: otherAuthInfo{}})
	if _, _, ok := peerID(ctx); ok {
		t.Error("a non-peerAuth AuthInfo must report unknown, not panic or misread")
	}
}

// otherAuthInfo is a stand-in for some OTHER credentials.AuthInfo
// implementation, to prove peerID's type assertion fails closed rather than
// panicking or misreading foreign auth info.
type otherAuthInfo struct{ credentials.CommonAuthInfo }

func (otherAuthInfo) AuthType() string { return "other" }
