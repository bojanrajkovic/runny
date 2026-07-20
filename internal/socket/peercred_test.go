package socket

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

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

// TestPeerCredsCarriesUIDOverRealSocket proves the platform chain end to end:
// a real unix socket (bufconn has no fd, so SyscallConn().Control has nothing
// to read), the server installed with peerCreds, and an insecure client (the
// one every runny client actually is) still completing the handshake and
// landing the caller's uid in the RPC context.
func TestPeerCredsCarriesUIDOverRealSocket(t *testing.T) {
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

	if _, err := client.GetStatus(t.Context(), &runnyv1.GetStatusRequest{}); err != nil {
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
	// the real connecting uid (this test process's own).
	if runtime.GOOS != "darwin" {
		if auth.UID != nil {
			t.Errorf("non-darwin stub must report unknown, got uid %d", *auth.UID)
		}
		return
	}
	if auth.UID == nil {
		t.Fatal("darwin must read the peer uid, got unknown")
	}
	if *auth.UID != uint32(os.Getuid()) {
		t.Errorf("uid = %d, want this process's uid %d", *auth.UID, os.Getuid())
	}
}

func TestPeerUID(t *testing.T) {
	uid, ok := peerUID(t.Context())
	if ok {
		t.Errorf("no peer in context: expected unknown, got uid %d", uid)
	}

	want := uint32(42)
	ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{UID: &want}})
	if uid, ok := peerUID(ctx); !ok || uid != 42 {
		t.Errorf("peerUID = (%d, %v), want (42, true)", uid, ok)
	}

	ctx = peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{UID: nil}})
	if _, ok := peerUID(ctx); ok {
		t.Error("nil UID must report unknown")
	}

	ctx = peer.NewContext(t.Context(), &peer.Peer{AuthInfo: otherAuthInfo{}})
	if _, ok := peerUID(ctx); ok {
		t.Error("a non-peerAuth AuthInfo must report unknown, not panic or misread")
	}
}

// otherAuthInfo is a stand-in for some OTHER credentials.AuthInfo
// implementation, to prove peerUID's type assertion fails closed rather than
// panicking or misreading foreign auth info.
type otherAuthInfo struct{ credentials.CommonAuthInfo }

func (otherAuthInfo) AuthType() string { return "other" }
