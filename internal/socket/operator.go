package socket

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/user"
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
	if s.HomeDir.String() != home.SystemHomeDir {
		return status.Error(codes.FailedPrecondition,
			"operator grants require the system daemon (the per-user daemon has a single owner)")
	}
	return nil
}

// resolveAccountTimeout bounds resolveOperatorAccount: os/user has no
// context-aware lookup, and a directory-service-backed NSS can stall.
// Unlike lookupUsername's audit-only best-effort skip, this result is
// required for GrantOperator/RevokeOperator to proceed at all, so a stuck
// lookup surfaces as a clear timeout rather than silently degrading.
const resolveAccountTimeout = 5 * time.Second

// errAccountLookupTimeout distinguishes "the lookup itself never returned"
// from a genuine "no such account", so the RPC handler can report the
// former as a retryable condition rather than a wrong argument.
var errAccountLookupTimeout = errors.New("account resolution timed out")

// userLookupByName/userLookupByID seam over os/user, overridable in tests to
// simulate a stuck NSS lookup without a real syscall.
var (
	userLookupByName = user.Lookup
	userLookupByID   = user.LookupId
)

type accountLookupResult struct {
	u   *user.User
	err error
}

// boundedAccountLookup runs fn (a name or uid os/user lookup) with a bound,
// abandoning the goroutine on timeout — accepted here because
// GrantOperator/RevokeOperator are rare, human-initiated admin actions, not
// a path that could pile up abandoned goroutines under automated load the
// way InjectDebugKey's audit lookup could.
func boundedAccountLookup(fn func() (*user.User, error)) (accountLookupResult, bool) {
	ch := make(chan accountLookupResult, 1)
	go func() {
		u, err := fn()
		ch <- accountLookupResult{u, err}
	}()
	select {
	case r := <-ch:
		return r, true
	case <-time.After(resolveAccountTimeout):
		return accountLookupResult{}, false
	}
}

// resolveOperatorAccount looks up user by name first, then (if it parses as
// a uint32) by uid — the "name or uid" contract GrantOperator/
// RevokeOperator's user field promises.
func resolveOperatorAccount(input string) (*user.User, error) {
	r, ok := boundedAccountLookup(func() (*user.User, error) { return userLookupByName(input) })
	if !ok {
		return nil, errAccountLookupTimeout
	}
	if r.err == nil {
		return r.u, nil
	}
	if _, err := strconv.ParseUint(input, 10, 32); err == nil {
		r, ok := boundedAccountLookup(func() (*user.User, error) { return userLookupByID(input) })
		if !ok {
			return nil, errAccountLookupTimeout
		}
		if r.err == nil {
			return r.u, nil
		}
	}
	return nil, fmt.Errorf("no such user %q", input)
}

// resolveOperatorAccountOrStatus wraps resolveOperatorAccount's result as a
// gRPC status: a genuine lookup miss is InvalidArgument (a wrong argument),
// while a stuck lookup is Unavailable (a retryable condition, not the
// caller's fault) — distinct codes so a client can tell "fix your input"
// from "try again".
func resolveOperatorAccountOrStatus(input string) (*user.User, error) {
	u, err := resolveOperatorAccount(input)
	switch {
	case err == nil:
		return u, nil
	case errors.Is(err, errAccountLookupTimeout):
		return nil, status.Errorf(codes.Unavailable, "resolving %q timed out; the directory service may be unavailable — try again", input)
	default:
		return nil, status.Errorf(codes.InvalidArgument, "no such user %q", input)
	}
}

// operatorIdentity resolves the calling peer's uid/username for grant
// attribution, the same identity InjectDebugKey stamps onto injected_keys.
func operatorIdentity(ctx context.Context) (uid uint32, username string) {
	if u, ok := peerUID(ctx); ok {
		return u, lookupUsername(u)
	}
	return 0, ""
}

func (s *Server) GrantOperator(ctx context.Context, req *runnyv1.GrantOperatorRequest) (*runnyv1.OperatorMutation, error) {
	if err := s.requireSystemDaemon(); err != nil {
		return nil, err
	}
	return s.grantOperator(ctx, req.GetUser())
}

func (s *Server) grantOperator(ctx context.Context, userArg string) (*runnyv1.OperatorMutation, error) {
	return s.mutateOperator(
		ctx, userArg, "grant", "granting",
		func(ops []opacl.Operator, uid uint32, u *user.User) error {
			if u.Username == "root" || u.Uid == "0" {
				return status.Error(codes.InvalidArgument, "refusing to grant root")
			}
			for _, op := range ops {
				if op.UID == uid {
					return status.Errorf(codes.FailedPrecondition, "%s is already an operator", u.Username)
				}
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
	return s.revokeOperator(ctx, req.GetUser())
}

func (s *Server) revokeOperator(ctx context.Context, userArg string) (*runnyv1.OperatorMutation, error) {
	return s.mutateOperator(
		ctx, userArg, "revoke", "revoking",
		func(ops []opacl.Operator, uid uint32, u *user.User) error {
			found := false
			for _, op := range ops {
				if op.UID == uid {
					found = true
					break
				}
			}
			if !found {
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
// and append an attribution record. recordAction ("grant"/"revoke") is the
// operator-grants.jsonl verb; verbing ("granting"/"revoking") is only for
// the apply-failure error message.
func (s *Server) mutateOperator(
	ctx context.Context, userArg, recordAction, verbing string,
	precheck func(ops []opacl.Operator, uid uint32, u *user.User) error,
	apply func(actx bounded.Context, homeDir, sock, username string) error,
) (*runnyv1.OperatorMutation, error) {
	u, err := resolveOperatorAccountOrStatus(userArg)
	if err != nil {
		return nil, err
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "account %s has a non-numeric uid %q", u.Username, u.Uid)
	}
	uid := uint32(uid64)

	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading the operator set: %v", err)
	}
	if err := precheck(ops, uid, u); err != nil {
		return nil, err
	}

	actx, cancel := bounded.WithTimeout(ctx, aclOpTimeout)
	defer cancel()
	if err := apply(actx, s.HomeDir.String(), s.socketPath, u.Username); err != nil {
		return nil, status.Errorf(codes.Internal, "%s %s: %v", verbing, u.Username, err)
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
