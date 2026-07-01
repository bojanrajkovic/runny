package socket

import (
	"context"
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

// resolveOperatorAccount looks up user by name first, then (if it parses as
// a uint32) by uid — the "name or uid" contract GrantOperator/
// RevokeOperator's user field promises.
func resolveOperatorAccount(input string) (*user.User, error) {
	if u, err := user.Lookup(input); err == nil {
		return u, nil
	}
	if _, err := strconv.ParseUint(input, 10, 32); err == nil {
		if u, err := user.LookupId(input); err == nil {
			return u, nil
		}
	}
	return nil, fmt.Errorf("no such user %q", input)
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
	u, err := resolveOperatorAccount(userArg)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "no such user %q", userArg)
	}
	if u.Username == "root" || u.Uid == "0" {
		return nil, status.Error(codes.InvalidArgument, "refusing to grant root")
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
	for _, op := range ops {
		if op.UID == uid {
			return nil, status.Errorf(codes.FailedPrecondition, "%s is already an operator", u.Username)
		}
	}

	actx, cancel := bounded.WithTimeout(ctx, aclOpTimeout)
	defer cancel()
	if err := opacl.Grant(actx, s.HomeDir.String(), s.socketPath, u.Username); err != nil {
		return nil, status.Errorf(codes.Internal, "granting %s: %v", u.Username, err)
	}

	byUID, byUser := operatorIdentity(ctx)
	rec := home.OperatorGrant{
		Action: "grant", ByUID: byUID, ByUser: byUser,
		TargetUID: uid, TargetUser: u.Username, At: time.Now(),
	}
	if err := s.HomeDir.AppendOperatorGrant(rec); err != nil {
		slog.Error("operator grant: attribution record not written", "target", u.Username, "err", err)
	}
	return &runnyv1.OperatorMutation{Uid: uid, User: u.Username}, nil
}

func (s *Server) RevokeOperator(ctx context.Context, req *runnyv1.RevokeOperatorRequest) (*runnyv1.OperatorMutation, error) {
	if err := s.requireSystemDaemon(); err != nil {
		return nil, err
	}
	return s.revokeOperator(ctx, req.GetUser())
}

func (s *Server) revokeOperator(ctx context.Context, userArg string) (*runnyv1.OperatorMutation, error) {
	u, err := resolveOperatorAccount(userArg)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "no such user %q", userArg)
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
	found := false
	for _, op := range ops {
		if op.UID == uid {
			found = true
			break
		}
	}
	if !found {
		return nil, status.Errorf(codes.FailedPrecondition, "%s is not an operator", u.Username)
	}
	if len(ops) <= 1 {
		return nil, status.Error(codes.FailedPrecondition,
			"refusing to revoke the last operator — recover with: sudo runnyctl install-daemon")
	}

	actx, cancel := bounded.WithTimeout(ctx, aclOpTimeout)
	defer cancel()
	if err := opacl.Revoke(actx, s.HomeDir.String(), s.socketPath, u.Username); err != nil {
		return nil, status.Errorf(codes.Internal, "revoking %s: %v", u.Username, err)
	}

	byUID, byUser := operatorIdentity(ctx)
	rec := home.OperatorGrant{
		Action: "revoke", ByUID: byUID, ByUser: byUser,
		TargetUID: uid, TargetUser: u.Username, At: time.Now(),
	}
	if err := s.HomeDir.AppendOperatorGrant(rec); err != nil {
		slog.Error("operator revoke: attribution record not written", "target", u.Username, "err", err)
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
