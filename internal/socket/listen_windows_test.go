//go:build windows

package socket

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	winio "github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// uniquePipeName returns a per-test pipe path: ListenPipe fails if the name
// already exists, so repeated or parallel runs must not collide on the fixed
// production name.
func uniquePipeName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\runnyd-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
}

// TestPipeTransportRoundTrip proves the windows control channel end to end: the
// per-platform listen() binds an overlapped named pipe, a gRPC server serves
// over it, and a client dialing at SECURITY_IDENTIFICATION completes a real
// GetStatus RPC — the async winio net.Conn carries HTTP/2 correctly.
func TestPipeTransportRoundTrip(t *testing.T) {
	name := uniquePipeName(t)
	ln, err := listen(name, true)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	g := grpc.NewServer()
	runnyv1.RegisterRunnyServiceServer(g, newTestServer(testSlots("mac-1"), nil, nil, nil))
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient("passthrough:runnyd",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeAccessImpLevel(ctx, name, pipeDialAccessTest, winio.PipeImpLevelIdentification)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	resp, err := runnyv1.NewRunnyServiceClient(conn).GetStatus(t.Context(), &runnyv1.GetStatusRequest{})
	if err != nil {
		t.Fatalf("GetStatus over pipe: %v", err)
	}
	if len(resp.GetSlots()) != 1 {
		t.Fatalf("slots = %d, want 1", len(resp.GetSlots()))
	}
}

// pipeDialAccessTest is GENERIC_READ|GENERIC_WRITE, matching runnyctl's client
// dial access (dial_windows.go's pipeDialAccess, which lives in package main).
const pipeDialAccessTest = 0x80000000 | 0x40000000

// TestPipeSDDL pins the per-daemon connect DACL: the system daemon grants
// Authenticated Users; a per-user daemon grants only the resolving user's own
// SID (owner-only), so the descriptor names that SID and not AU.
func TestPipeSDDL(t *testing.T) {
	sys, err := pipeSDDL(true)
	if err != nil {
		t.Fatalf("pipeSDDL(system): %v", err)
	}
	if sys != authenticatedUsersSDDL {
		t.Errorf("system pipe SDDL = %q, want %q", sys, authenticatedUsersSDDL)
	}

	user, err := pipeSDDL(false)
	if err != nil {
		t.Fatalf("pipeSDDL(per-user): %v", err)
	}
	if user == authenticatedUsersSDDL {
		t.Error("per-user pipe SDDL must not grant Authenticated Users")
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	if want := "D:(A;;GA;;;" + tu.User.Sid.String() + ")"; user != want {
		t.Errorf("per-user pipe SDDL = %q, want %q (owner-only)", user, want)
	}
}
