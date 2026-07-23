package socket

import (
	"context"
	"errors"
	"log/slog"
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
	id     string
	cancel context.CancelCauseFunc
}

// errRevoked is the CancelCauseFunc cause killStreams cancels with,
// distinguishing a revoke-kill from every other reason a stream's context
// ends (client disconnect, RPC deadline, normal return).
var errRevoked = errors.New("operator revoked")

// newOperatorGate returns an armed gate reading homeDir's ACL, or nil when
// armed is false (a per-user daemon has no ACL-managed operator set to
// enforce against) or when the peerCredSupported arming check fails (this
// platform has no working peer-identity read to enforce against -- arming
// anyway would deny every RPC, since check/stream fail closed on an
// unreadable identity; see peerCredSupported's per-platform doc comments).
// The && ordering is load-bearing: the arming check runs only when arming
// is actually requested, because on Windows it is a live socket self-probe.
// A nil *operatorGate is pass-through everywhere below, falling back to the
// socket-is-the-sole-gate baseline. Arming is announced at INFO — the
// visible confirmation that per-RPC revocation is enforced with a real
// peer-identity read, mirroring the loud per-platform log when the check
// fails.
func newOperatorGate(armed bool, homeDir string) *operatorGate {
	if !armed || !peerCredSupported() {
		return nil
	}
	slog.Info("operator revocation gate armed: per-RPC checks enforced with a kernel-authenticated peer-identity read")
	return &operatorGate{homeDir: homeDir, streams: newFanoutRegistry[gateStream]()}
}

// check is the shared identity → verdict, resolving the identity from ctx
// first: fail closed (PermissionDenied) when the peer identity could not be
// read; nil for the platform's always-authorized principal (the Privileged
// verdict readPeerID attached at handshake — root on darwin, SYSTEM or an
// elevated Administrators member on Windows — which bypasses the socket's
// access control by design and holds no ACE); else checkID's verdict.
func (g *operatorGate) check(ctx context.Context) error {
	id, privileged, ok := peerID(ctx)
	if !ok {
		return status.Error(codes.PermissionDenied, "operator revocation check: peer identity could not be read")
	}
	if privileged {
		return nil
	}
	return g.checkID(id)
}

// checkID is the ACL half of check, for callers that already resolved the
// identity and consulted the privileged verdict: PermissionDenied for any
// identity absent from a fresh, uncached ListIDs read. Membership is plain
// string equality against the platform-native identities the ACL read
// returns.
func (g *operatorGate) checkID(id string) error {
	ids, err := opacl.ListIDs(g.homeDir)
	if err != nil {
		return status.Errorf(codes.PermissionDenied, "operator revocation check: reading the operator set: %v", err)
	}
	if slices.Contains(ids, id) {
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
// (id, cancel) BEFORE calling check — the ordering that closes the
// open-vs-revoke race: a stream opening concurrently with a revoke either
// fails the (post-chmod) check below, or is already registered when the
// sweep runs and gets cancelled. No interleaving lets a stream slip
// through. When the handler returns because killStreams cancelled it, that
// is converted to PermissionDenied instead of the handler's nil —
// visible-not-silent.
func (g *operatorGate) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	id, privileged, ok := peerID(ss.Context())
	if !ok {
		return status.Error(codes.PermissionDenied, "operator revocation check: peer identity could not be read")
	}

	ctx, cancel := context.WithCancelCause(ss.Context())
	wrapped := wrappedStream{ServerStream: ss, ctx: ctx}
	defer g.streams.register(gateStream{id: id, cancel: cancel})()
	defer cancel(nil) // release the child context's resources either way

	// The privileged principal skips the ACL check for the same reason
	// check does: it holds no ACE, so a membership read could only deny it.
	if !privileged {
		if err := g.checkID(id); err != nil {
			return err
		}
	}
	err := handler(srv, wrapped)
	if err == nil && errors.Is(context.Cause(ctx), errRevoked) {
		return status.Error(codes.PermissionDenied, "operator revoked")
	}
	return err
}

// killStreams cancels every registered stream owned by id. Called by the
// revoke path after the ACL mutation lands. Safe on a nil gate (a per-user
// daemon, or a test Server that never called Serve).
func (g *operatorGate) killStreams(id string) {
	if g == nil {
		return
	}
	g.streams.forEach(func(s gateStream) {
		if s.id == id {
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
