package socket

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"
	"slices"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/opacl"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// aclOpTimeout bounds every opacl Grant/Revoke call: a local chmod is fast,
// but "no unbounded operations" applies to every daemon-facing RPC, not
// just guest-facing ones.
const aclOpTimeout = 10 * time.Second

// requireSystemDaemon refuses operator grant/revoke on a per-user daemon,
// which has a single owner and no ACL-managed set to mutate. Read-only
// ListOperators is not gated: it simply reports whatever ACL is present.
func (s *Server) requireSystemDaemon() error {
	if !s.IsSystemDaemon {
		return status.Error(codes.FailedPrecondition,
			"operator grants require the system daemon (the per-user daemon has a single owner)")
	}
	return nil
}

// resolveOperatorAccount looks up user by name first, then (if it parses as
// a uint32) by uid — the "name or uid" contract GrantOperator/
// RevokeOperator's user field promises. os/user has no context-aware
// variant, but this is a local, human-initiated admin lookup against local
// accounts; a stall would need a domain-joined Mac with a wedged directory
// service, which is a bug report, not the unbounded-guest failure mode the
// project's bounded-context invariant exists to kill.
func resolveOperatorAccount(input string) (*user.User, error) {
	u, err := user.Lookup(input)
	if err == nil {
		return u, nil
	}
	lookupErr := err
	if _, perr := strconv.ParseUint(input, 10, 32); perr == nil {
		if u, err := user.LookupId(input); err == nil {
			return u, nil
		} else {
			lookupErr = err
		}
	}
	return nil, fmt.Errorf("no such user %q: %w", input, lookupErr)
}

// operatorIdentity resolves the calling peer's kernel-authenticated uid and
// best-effort username — the one resolver behind grant attribution,
// injected-key audit rows, and lifecycle-command log lines. A nil uid means
// the peer cred could not be read, never conflated with root's real uid 0.
func operatorIdentity(ctx context.Context) (uid *uint32, username string) {
	u, ok := peerUID(ctx)
	if !ok {
		return nil, ""
	}
	return &u, lookupUsername(u)
}

func (s *Server) GrantOperator(ctx context.Context, req *runnyv1.GrantOperatorRequest) (*runnyv1.OperatorMutation, error) {
	if err := s.requireSystemDaemon(); err != nil {
		return nil, err
	}
	return s.mutateOperator(
		ctx, req.GetUser(), "grant",
		func(ops []opacl.Operator, uid uint32, u *user.User) error {
			if u.Username == "root" || u.Uid == "0" {
				return status.Error(codes.InvalidArgument, "refusing to grant root")
			}
			if opacl.ContainsUID(ops, uid) {
				return status.Errorf(codes.FailedPrecondition, "%s is already an operator", u.Username)
			}
			return nil
		},
		opacl.Grant,
	)
}

func (s *Server) RevokeOperator(ctx context.Context, req *runnyv1.RevokeOperatorRequest) (*runnyv1.OperatorMutation, error) {
	if err := s.requireSystemDaemon(); err != nil {
		return nil, err
	}
	return s.mutateOperator(
		ctx, req.GetUser(), "revoke",
		func(ops []opacl.Operator, uid uint32, u *user.User) error {
			if !opacl.ContainsUID(ops, uid) {
				return status.Errorf(codes.FailedPrecondition, "%s is not an operator", u.Username)
			}
			if len(ops) <= 1 {
				return status.Error(codes.FailedPrecondition,
					"refusing to revoke the last operator — recover with: sudo runnyctl install-daemon")
			}
			return nil
		},
		opacl.Revoke,
	)
}

// mutateOperator is the shared grant/revoke skeleton: resolve the account,
// list the current operator set, run precheck (which returns the
// grant-only "already an operator"/refuse-root or revoke-only "not an
// operator"/last-operator errors), apply the opacl mutation under a bound,
// and append an attribution record. recordAction ("grant"/"revoke") is both
// the operator-grants.jsonl verb and the apply-failure error message's verb.
func (s *Server) mutateOperator(
	ctx context.Context, userArg, recordAction string,
	precheck func(ops []opacl.Operator, uid uint32, u *user.User) error,
	apply func(actx bounded.Context, homeDir, sock, username string) error,
) (*runnyv1.OperatorMutation, error) {
	u, err := resolveOperatorAccount(userArg)
	if err != nil {
		// InvalidArgument: a lookup miss is a wrong argument, not the
		// caller's transient fault.
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "account %s has a non-numeric uid %q: %v", u.Username, u.Uid, err)
	}
	uid := uint32(uid64)

	// The List-then-mutate sequence below must run as one unit: without this
	// lock, two concurrent grant/revoke RPCs (gRPC dispatches unary calls on
	// separate goroutines) could both read the same pre-mutation operator
	// set and both pass a precondition (e.g. "not the last operator") the
	// other's mutation has already invalidated.
	s.operatorMu.Lock()
	defer s.operatorMu.Unlock()

	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading the operator set: %v", err)
	}
	if err := precheck(ops, uid, u); err != nil {
		return nil, err
	}

	// context.Background(), not ctx: the two-chmod ACL mutation is a local,
	// daemon-owned operation that must run to completion, so a client
	// disconnecting mid-flight can't leave the ACL half-stamped. Still
	// bounded by aclOpTimeout.
	actx, cancel := bounded.WithTimeout(context.Background(), aclOpTimeout)
	defer cancel()
	applyErr := apply(actx, s.HomeDir.String(), s.socketPath, u.Username)
	if recordAction == "revoke" {
		// Ground truth, not applyErr: chmodBoth's two chmod calls (home dir,
		// then the live socket) aren't atomic, so a failure on the second can
		// still leave the first — the ACL checkUID actually reads — already
		// mutated. Re-reading here means that partial failure still kills the
		// operator's live streams, instead of their next RPC being silently
		// denied while an already-open WatchStatus/StreamLogs lingers.
		if uids, err := opacl.ListUIDs(s.HomeDir.String()); err == nil && !slices.Contains(uids, uid) {
			s.gate.killStreams(uid)
		}
	}
	if applyErr != nil {
		return nil, status.Errorf(codes.Internal, "%s failed for %s: %v", recordAction, u.Username, applyErr)
	}

	byUID, byUser := operatorIdentity(ctx)
	rec := home.OperatorGrant{
		Action: recordAction, ByUID: byUID, ByUser: byUser,
		TargetUID: uid, TargetUser: u.Username, At: time.Now(),
	}
	if err := s.HomeDir.AppendOperatorGrant(rec); err != nil {
		slog.Error("operator "+recordAction+": attribution record not written", "target", u.Username, "err", err)
	}
	return &runnyv1.OperatorMutation{Uid: uid, User: u.Username}, nil
}

func (s *Server) ListOperators(_ context.Context, _ *runnyv1.ListOperatorsRequest) (*runnyv1.ListOperatorsResponse, error) {
	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading the operator set: %v", err)
	}
	grants, err := s.HomeDir.ReadOperatorGrants()
	if err != nil {
		slog.Warn("listing operators: grant attribution log unreadable; showing operators with no attribution", "err", err)
	}
	resp := &runnyv1.ListOperatorsResponse{}
	for _, op := range ops {
		e := &runnyv1.Operator{Uid: op.UID, User: op.User}
		if g := latestGrant(grants, op.UID); g != nil {
			e.GrantedBy = g.ByUser
			e.GrantedAt = timestamppb.New(g.At)
		}
		resp.Operators = append(resp.Operators, e)
	}
	return resp, nil
}

// latestGrant returns the most recent "grant" record for uid, or nil if
// none exists (the install-time bootstrap operator, shown as "(install)").
func latestGrant(grants []home.OperatorGrant, uid uint32) *home.OperatorGrant {
	var latest *home.OperatorGrant
	for i := range grants {
		g := &grants[i]
		if g.TargetUID != uid || g.Action != "grant" {
			continue
		}
		if latest == nil || g.At.After(latest.At) {
			latest = g
		}
	}
	return latest
}
