package socket

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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

	// darwin and windows must resolve the real connecting identity (this
	// test process's own); the remaining-platform stub always reports
	// unknown. The exact-SID assertion for windows lives in the
	// windows-tagged readPeerID test, which has the token APIs in reach.
	switch runtime.GOOS {
	case "darwin":
		if auth.ID == nil {
			t.Fatal("darwin must read the peer identity, got unknown")
		}
		if want := strconv.Itoa(os.Getuid()); *auth.ID != want {
			t.Errorf("id = %q, want this process's uid %q", *auth.ID, want)
		}
		if want := os.Getuid() == 0; auth.Privileged != want {
			t.Errorf("Privileged = %v, want %v (uid %d)", auth.Privileged, want, os.Getuid())
		}
	case "windows":
		if auth.ID == nil {
			t.Fatal("windows must read the peer identity, got unknown")
		}
		if !strings.HasPrefix(*auth.ID, "S-1-") {
			t.Errorf("id = %q, want a SID string", *auth.ID)
		}
	default:
		if auth.ID != nil {
			t.Errorf("the remaining-platform stub must report unknown, got id %q", *auth.ID)
		}
	}
}

func TestPeerID(t *testing.T) {
	id, _, ok := peerID(t.Context())
	if ok {
		t.Errorf("no peer in context: expected unknown, got id %q", id)
	}

	// Both identity shapes pass through verbatim — a darwin decimal uid and a
	// Windows SID are opaque strings to the transport layer — and the
	// privileged verdict travels beside the identity, never re-derived from
	// the string ("0" with the flag unset stays unprivileged).
	for _, c := range []struct {
		id         string
		privileged bool
	}{
		{"42", false},
		{"S-1-5-21-1111111111-2222222222-3333333333-1001", false},
		{"0", true},
		{"0", false},
		{"S-1-5-18", true},
	} {
		w := c.id
		ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{ID: &w, Privileged: c.privileged}})
		if id, privileged, ok := peerID(ctx); !ok || id != c.id || privileged != c.privileged {
			t.Errorf("peerID = (%q, %v, %v), want (%q, %v, true)", id, privileged, ok, c.id, c.privileged)
		}
	}

	ctx := peer.NewContext(t.Context(), &peer.Peer{AuthInfo: peerAuth{ID: nil}})
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
