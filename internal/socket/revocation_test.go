//go:build darwin

package socket

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/opacl"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// newGateTestHome builds a fresh temp home dir (with a stand-in socket file,
// mirroring Grant/Revoke's live stamp target) and returns its path plus an
// armed *operatorGate reading it.
func newGateTestHome(t *testing.T) (homeDir string, g *operatorGate) {
	t.Helper()
	dir := t.TempDir()
	homeDir = filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(homeDir, "runnyd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return homeDir, newOperatorGate(true, homeDir)
}

func granteeUID(t *testing.T) uint32 {
	t.Helper()
	u, err := user.Lookup(testGrantee1)
	if err != nil {
		t.Skipf("test principal %q not present on this host: %v", testGrantee1, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		t.Fatalf("parsing uid %q: %v", u.Uid, err)
	}
	return uint32(uid)
}

func grantSock(homeDir string) string { return filepath.Join(homeDir, "runnyd.sock") }

// TestOperatorGateUnarmedWhenNotSystemDaemon pins that a per-user daemon
// (no ACL-managed operator set) never arms the gate — newOperatorGate
// returns nil, and nil is pass-through everywhere it's consulted.
func TestOperatorGateUnarmedWhenNotSystemDaemon(t *testing.T) {
	if g := newOperatorGate(false, "/irrelevant"); g != nil {
		t.Fatalf("newOperatorGate(false, ...) = %v, want nil", g)
	}
}

// TestOperatorGateRootAlwaysPasses pins that uid 0 bypasses the ACL check
// entirely (root bypasses the socket's 0600 mode by design and holds no
// ACE, so it can never appear in ListUIDs).
func TestOperatorGateRootAlwaysPasses(t *testing.T) {
	_, g := newGateTestHome(t)
	ctx := asOperator(t.Context(), 0)
	if err := g.check(ctx); err != nil {
		t.Fatalf("check(uid 0) = %v, want nil", err)
	}
}

// TestOperatorGateUnknownUIDDenied pins fail-closed: a gated RPC whose peer
// uid could not be read is denied, never allowed through.
func TestOperatorGateUnknownUIDDenied(t *testing.T) {
	_, g := newGateTestHome(t)
	err := g.check(t.Context()) // no peer in context at all
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
}

// TestOperatorGateUnaryDeniesPostRevoke pins the headline fix for #221: a
// unary RPC over an already-held connection is denied on the very next
// call after the operator is revoked from the ACL — connect()-time state
// is never consulted.
func TestOperatorGateUnaryDeniesPostRevoke(t *testing.T) {
	homeDir, g := newGateTestHome(t)
	uid := granteeUID(t)
	ctx := asOperator(t.Context(), uid)

	actx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := opacl.Grant(actx, homeDir, grantSock(homeDir), testGrantee1); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if err := g.check(ctx); err != nil {
		t.Fatalf("check while granted = %v, want nil", err)
	}

	called := false
	handler := func(ctx context.Context, req any) (any, error) { called = true; return "ok", nil }
	if _, err := g.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test/Grant"}, handler); err != nil {
		t.Fatalf("unary while granted = %v, want nil", err)
	}
	if !called {
		t.Fatal("handler not invoked while granted")
	}

	if err := opacl.Revoke(actx, homeDir, grantSock(homeDir), testGrantee1); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	called = false
	_, err := g.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/test/Revoked"}, handler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code after revoke = %v, want PermissionDenied", status.Code(err))
	}
	if called {
		t.Fatal("handler invoked for a revoked operator")
	}
}

// fakeServerStream is the minimal grpc.ServerStream a stream interceptor
// test needs: only Context() is exercised by operatorGate.stream.
type fakeServerStream struct {
	ctx context.Context
}

func (f *fakeServerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeServerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeServerStream) SetTrailer(metadata.MD)       {}
func (f *fakeServerStream) Context() context.Context     { return f.ctx }
func (f *fakeServerStream) SendMsg(m any) error          { return nil }
func (f *fakeServerStream) RecvMsg(m any) error          { return nil }

// TestOperatorGateStreamKilledOnRevoke pins the in-flight-stream-kill
// mechanism: a stream already registered before a revoke is cancelled by
// killStreams, and the interceptor reports PermissionDenied rather than
// letting the handler's nil (a silent clean close) reach the client.
func TestOperatorGateStreamKilledOnRevoke(t *testing.T) {
	homeDir, g := newGateTestHome(t)
	uid := granteeUID(t)
	ctx := asOperator(t.Context(), uid)

	actx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := opacl.Grant(actx, homeDir, grantSock(homeDir), testGrantee1); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	ss := &fakeServerStream{ctx: ctx}
	handlerEntered := make(chan struct{})
	handler := func(srv any, stream grpc.ServerStream) error {
		close(handlerEntered)
		<-stream.Context().Done() // mirrors WatchStatus/StreamLogs's select on Done()
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- g.stream(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test/Watch"}, handler)
	}()

	<-handlerEntered
	// Revoke without killing anything: mirrors an out-of-band ACL edit —
	// not exercised here, only that killStreams is what actually cancels.
	if err := opacl.Revoke(actx, homeDir, grantSock(homeDir), testGrantee1); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	g.killStreams(uid)

	select {
	case err := <-errCh:
		if status.Code(err) != codes.PermissionDenied {
			t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream was not cancelled by killStreams")
	}
}

// TestOperatorGateStreamDeregistersOnDenial pins that a stream whose ACL
// check fails (never granted, so registered-then-denied — the same path a
// stream opening concurrently with a revoke takes) is cleaned out of the
// registry rather than leaking an entry killStreams could spuriously act
// on later for a reused id.
func TestOperatorGateStreamDeregistersOnDenial(t *testing.T) {
	_, g := newGateTestHome(t) // fresh ACL, testGrantee1 never granted
	uid := granteeUID(t)
	ctx := asOperator(t.Context(), uid)

	handler := func(srv any, stream grpc.ServerStream) error {
		t.Fatal("handler invoked for a stream that should have been denied")
		return nil
	}
	ss := &fakeServerStream{ctx: ctx}
	err := g.stream(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test/Watch"}, handler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", status.Code(err))
	}
	if n := g.streams.len(); n != 0 {
		t.Fatalf("stream registry not cleaned up after a denied stream: %d entries", n)
	}
}

// TestMutateOperatorKillsStreamsOnPartialRevokeFailure pins the code-review
// fix for a real gap: opacl.Revoke's two chmod calls (home dir, then the
// live socket) aren't atomic. If the first succeeds and the second fails,
// mutateOperator used to skip killStreams entirely (gated behind apply
// returning nil) — so the revoked uid's *next* RPC was already denied (the
// home dir ACL, the one checkUID reads, was genuinely mutated) while any of
// its already-open streams lingered forever, the exact bug this whole
// change exists to close. mutateOperator now re-checks ground truth via
// ListUIDs after apply, regardless of apply's error, and kills on that.
func TestMutateOperatorKillsStreamsOnPartialRevokeFailure(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	s.gate = newOperatorGate(true, s.HomeDir.String())
	callerCtx := asOperator(t.Context(), 501)
	if _, err := s.GrantOperator(callerCtx, &runnyv1.GrantOperatorRequest{User: testGrantee1}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := s.GrantOperator(callerCtx, &runnyv1.GrantOperatorRequest{User: testGrantee2}); err != nil {
		t.Fatalf("grant 2 (so revoke isn't also the last-operator case): %v", err)
	}

	uid := granteeUID(t)
	streamCtx := asOperator(t.Context(), uid)
	ss := &fakeServerStream{ctx: streamCtx}
	handlerEntered := make(chan struct{})
	handler := func(srv any, stream grpc.ServerStream) error {
		close(handlerEntered)
		<-stream.Context().Done()
		return nil
	}
	streamErrCh := make(chan error, 1)
	go func() {
		streamErrCh <- s.gate.stream(nil, ss, &grpc.StreamServerInfo{FullMethod: "/test/Watch"}, handler)
	}()
	<-handlerEntered

	// A partial-failure apply: revokes the home dir ACE (the first, real
	// chmod — chmodBoth's actual first step), then fails on its own "second
	// step" without ever touching the socket, mirroring the live code's
	// homeDir-succeeded-sock-failed case using only exported opacl calls.
	partialFailApply := func(actx bounded.Context, homeDir, sock, username string) error {
		if err := opacl.Revoke(actx, homeDir, sock, username); err != nil {
			return err
		}
		return errors.New("simulated: the live-socket chmod failed")
	}
	_, err := s.mutateOperator(callerCtx, testGrantee1, "revoke", revokePrecheckForTest, partialFailApply)
	if status.Code(err) != codes.Internal {
		t.Fatalf("mutateOperator code = %v, want Internal (apply reported failure)", status.Code(err))
	}

	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if opacl.ContainsUser(ops, testGrantee1) {
		t.Fatalf("home-dir ACL should already be mutated despite the reported apply failure: %+v", ops)
	}

	select {
	case streamErr := <-streamErrCh:
		if status.Code(streamErr) != codes.PermissionDenied {
			t.Fatalf("stream code = %v, want PermissionDenied — killStreams should still have fired", status.Code(streamErr))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream was not killed despite the home-dir ACL already reflecting the revoke")
	}
}
