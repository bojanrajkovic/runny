//go:build windows

package socket

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
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

// pipeDialAccessTest mirrors runnyctl's pipeDialAccess (dial_windows.go, which
// lives in package main and so cannot be imported).
const pipeDialAccessTest = 0x80000000 | 0x40000000

// TestPipeSDDL pins the per-daemon connect DACL: the system daemon grants
// Authenticated Users behind an explicit O:BA owner (the client trusts only an
// Administrators/SYSTEM-owned system pipe); a per-user daemon grants only the
// resolving user's own SID (owner-only), so the descriptor names that SID and
// not AU.
func TestPipeSDDL(t *testing.T) {
	sys, err := pipeSDDL(true)
	if err != nil {
		t.Fatalf("pipeSDDL(system): %v", err)
	}
	// Literal, not compared to the const: this catches an edit that drops the
	// O:BA owner the client's dial trusts, which a self-referential
	// `sys != authenticatedUsersSDDL` check never could.
	tuSys, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	if want := "O:BAD:(A;;FA;;;" + tuSys.User.Sid.String() + ")(A;;0x12019b;;;AU)"; sys != want {
		t.Errorf("system pipe SDDL = %q, want %q", sys, want)
	}

	user, err := pipeSDDL(false)
	if err != nil {
		t.Fatalf("pipeSDDL(per-user): %v", err)
	}
	if strings.Contains(user, ";AU)") {
		t.Error("per-user pipe SDDL must not grant Authenticated Users")
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	// O: is pinned rather than left to the token default, and that is not
	// cosmetic: "System objects: Default owner for objects created by members
	// of the Administrators group" makes an elevated per-user daemon's pipe
	// owned by Administrators, which the client's per-user owner check reads
	// as a squat and refuses. Assert the property you create.
	sid := tu.User.Sid.String()
	if want := "O:" + sid + "D:(A;;GA;;;" + sid + ")"; user != want {
		t.Errorf("per-user pipe SDDL = %q, want %q (owner pinned, owner-only)", user, want)
	}
}

// TestClientGrantWithholdsInstanceCreation pins the one property the whole
// change exists for: whatever Authenticated Users are granted, it must not
// include FILE_CREATE_PIPE_INSTANCE.
//
// A pipe's security descriptor belongs to the NAME and is fixed by the first
// instance, so an instance added by another process inherits this owner — which
// is why the client's owner check cannot detect one, and why the DACL is the
// only place the squat can be stopped. Measured on Windows: granting GRGW here
// instead of the explicit mask lets an unprivileged principal add an instance,
// so a future "simplification" back to generic rights is a real regression and
// not a cosmetic one.
func TestClientGrantWithholdsInstanceCreation(t *testing.T) {
	if clientAccessMask&fileCreatePipeInstance != 0 {
		t.Errorf("clientAccessMask 0x%x grants FILE_CREATE_PIPE_INSTANCE — any authenticated user could add an instance to the live pipe", clientAccessMask)
	}
	sys, err := pipeSDDL(true)
	if err != nil {
		t.Fatalf("pipeSDDL(system): %v", err)
	}
	if strings.Contains(sys, "GA;;;AU") || strings.Contains(sys, "FA;;;AU") || strings.Contains(sys, "GRGW;;;AU") {
		t.Errorf("system pipe SDDL %q grants Authenticated Users a generic right that carries instance creation", sys)
	}
}
