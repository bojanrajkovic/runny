package socket

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/opacl"
)

// operatorGate is the per-RPC revocation check (armed only on the system
// daemon) plus the live-stream registry the revoke sweep cancels through.
// It moves enforcement from connect()-time-only to RPC-start on every call:
// a revoked operator holding an already-open connection is denied on its
// next RPC, and any stream it already had open is actively killed. Testable
// on its own (a fake homeDir, no gRPC server) — only the wiring line in
// Serve and the sweep call in mutateOperator touch Server.
type operatorGate struct {
	homeDir string
	streams *fanoutRegistry[gateStream] // live gated streams
}

type gateStream struct {
	uid    uint32
	cancel context.CancelCauseFunc
}

// errRevoked is the CancelCauseFunc cause killStreams cancels with,
// distinguishing a revoke-kill from every other reason a stream's context
// ends (client disconnect, RPC deadline, normal return).
var errRevoked = errors.New("operator revoked")

// newOperatorGate returns an armed gate reading homeDir's ACL, or nil when
// armed is false (a per-user daemon has no ACL-managed operator set to
// enforce against) or when peerCredSupported is false (this platform has no
// real peer-identity read to enforce against -- arming anyway would deny
// every RPC, since check/stream fail closed on an unreadable uid; see
// peerCredSupported's own doc comment). A nil *operatorGate is pass-through
// everywhere below, falling back to the socket-is-the-sole-gate baseline.
func newOperatorGate(armed bool, homeDir string) *operatorGate {
	if !armed || !peerCredSupported {
		return nil
	}
	return &operatorGate{homeDir: homeDir, streams: newFanoutRegistry[gateStream]()}
}

// check is the shared uid → verdict, resolving uid from ctx first: fail
// closed (PermissionDenied) when the peer uid could not be read, else
// checkUID's verdict.
func (g *operatorGate) check(ctx context.Context) error {
	uid, ok := peerUID(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "operator revocation check: peer uid could not be read")
	}
	return g.checkUID(uid)
}

// checkUID is check without the peer-uid lookup, for callers (stream) that
// already resolved it: nil for uid 0 (root bypasses the socket's 0600 mode
// by design and holds no ACE), PermissionDenied for any uid absent from a
// fresh, uncached ListUIDs read.
func (g *operatorGate) checkUID(uid uint32) error {
	if uid == 0 {
		return nil
	}
	uids, err := opacl.ListUIDs(g.homeDir)
	if err != nil {
		return status.Errorf(codes.PermissionDenied, "operator revocation check: reading the operator set: %v", err)
	}
	if slices.Contains(uids, uid) {
		return nil
	}
	return status.Error(codes.PermissionDenied, "operator revoked")
}

// unary is the ChainUnaryInterceptor entry, chained inside recoveryUnary
// (panic recovery stays outermost).
func (g *operatorGate) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := g.check(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// stream is the ChainStreamInterceptor entry. It registers the stream's
// (uid, cancel) BEFORE calling check — the ordering that closes the
// open-vs-revoke race: a stream opening concurrently with a revoke either
// fails the (post-chmod) check below, or is already registered when the
// sweep runs and gets cancelled. No interleaving lets a stream slip
// through. When the handler returns because killStreams cancelled it, that
// is converted to PermissionDenied instead of the handler's nil —
// visible-not-silent.
func (g *operatorGate) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	uid, ok := peerUID(ss.Context())
	if !ok {
		return status.Error(codes.PermissionDenied, "operator revocation check: peer uid could not be read")
	}

	ctx, cancel := context.WithCancelCause(ss.Context())
	wrapped := wrappedStream{ServerStream: ss, ctx: ctx}
	defer g.streams.register(gateStream{uid: uid, cancel: cancel})()
	defer cancel(nil) // release the child context's resources either way

	if err := g.checkUID(uid); err != nil {
		return err
	}
	err := handler(srv, wrapped)
	if err == nil && errors.Is(context.Cause(ctx), errRevoked) {
		return status.Error(codes.PermissionDenied, "operator revoked")
	}
	return err
}

// killStreams cancels every registered stream owned by uid. Called by the
// revoke path after the ACL mutation lands. Safe on a nil gate (a per-user
// daemon, or a test Server that never called Serve).
func (g *operatorGate) killStreams(uid uint32) {
	if g == nil {
		return
	}
	g.streams.forEach(func(s gateStream) {
		if s.uid == uid {
			s.cancel(errRevoked)
		}
	})
}

// wrappedStream overrides Context() with the cancellable child context;
// everything else delegates to the embedded ServerStream. Both gated
// stream handlers (WatchStatus, StreamLogs) already select on
// stream.Context().Done(), so this is the whole stream-kill mechanism —
// no handler changes needed.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }
