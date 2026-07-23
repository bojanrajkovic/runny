package socket

import (
	"context"
	"fmt"
	"log/slog"
	"os/user"
	"slices"
	"strconv"
	"strings"
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
// a uint32, or is SID-shaped) by id — the "name, uid, or SID" contract
// GrantOperator/RevokeOperator's user field promises. os/user has no
// context-aware variant, but this is a local, human-initiated admin lookup
// against local accounts; a stall would need a domain-joined host with a
// wedged directory service, which is a bug report, not the unbounded-guest
// failure mode the project's bounded-context invariant exists to kill.
func resolveOperatorAccount(input string) (*user.User, error) {
	u, err := user.Lookup(input)
	if err == nil {
		return u, nil
	}
	lookupErr := err
	_, perr := strconv.ParseUint(input, 10, 32)
	if perr == nil || strings.HasPrefix(input, "S-1-") {
		if u, err := user.LookupId(input); err == nil {
			return u, nil
		} else {
			lookupErr = err
		}
	}
	return nil, fmt.Errorf("no such user %q: %w", input, lookupErr)
}

// splitIdentity fans one platform-native identity string (os/user.User.Uid's
// convention: decimal uid on darwin, SID on Windows) out to the (uid, sid)
// field pair every operator-facing record and message carries: a numeric
// identity lands in the legacy uint32 uid field — darwin records stay
// byte-for-byte what they were before the sid fields existed — and anything
// else (including a numeric string too large for uint32, which no darwin
// uid is) lands in the sid field, lossless. "" (unknown identity) yields
// (nil, ""). No stamp site switches on the platform; the shape of the
// identity itself decides.
func splitIdentity(id string) (uid *uint32, sid string) {
	if id == "" {
		return nil, ""
	}
	if n, err := strconv.ParseUint(id, 10, 32); err == nil {
		u := uint32(n)
		return &u, ""
	}
	return nil, id
}

// operatorIdentity resolves the calling peer's kernel-authenticated
// platform-native identity and best-effort username — the one resolver
// behind grant attribution, injected-key audit rows, and lifecycle-command
// log lines. An empty id means the peer cred could not be read, never
// conflated with a real privileged peer ("0" on darwin is root, a real
// possible identity).
func operatorIdentity(ctx context.Context) (id, username string) {
	pid, ok := peerID(ctx)
	if !ok {
		return "", ""
	}
	return pid, lookupUsername(pid)
}

func (s *Server) GrantOperator(ctx context.Context, req *runnyv1.GrantOperatorRequest) (*runnyv1.OperatorMutation, error) {
	if err := s.requireSystemDaemon(); err != nil {
		return nil, err
	}
	return s.mutateOperator(
		ctx, req.GetUser(), "grant",
		func(ops []opacl.Operator, u *user.User) error {
			if err := refuseGrantTarget(u); err != nil {
				return err
			}
			if hasID(ops, u.Uid) {
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
		func(ops []opacl.Operator, u *user.User) error {
			if !hasID(ops, u.Uid) {
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
// grant-only "already an operator"/refused-target or revoke-only "not an
// operator"/last-operator errors), apply the opacl mutation under a bound,
// and append an attribution record. The account's identity is u.Uid
// verbatim — the platform-native string (decimal uid on darwin, SID on
// Windows) every ACL read and record comparison uses. recordAction
// ("grant"/"revoke") is both the operator-grants.jsonl verb and the
// apply-failure error message's verb.
func (s *Server) mutateOperator(
	ctx context.Context, userArg, recordAction string,
	precheck func(ops []opacl.Operator, u *user.User) error,
	apply func(actx bounded.Context, homeDir, sock, username string) error,
) (*runnyv1.OperatorMutation, error) {
	u, err := resolveOperatorAccount(userArg)
	if err != nil {
		// InvalidArgument: a lookup miss is a wrong argument, not the
		// caller's transient fault.
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

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
	if err := precheck(ops, u); err != nil {
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
		// Ground truth, not applyErr: the two-target ACL mutation (home dir,
		// then the live socket) isn't atomic, so a failure on the second can
		// still leave the first — the ACL checkID actually reads — already
		// mutated. Re-reading here means that partial failure still kills the
		// operator's live streams, instead of their next RPC being silently
		// denied while an already-open WatchStatus/StreamLogs lingers.
		if ids, err := opacl.ListIDs(s.HomeDir.String()); err == nil && !slices.Contains(ids, u.Uid) {
			s.gate.killStreams(u.Uid)
		}
	}
	if applyErr != nil {
		return nil, status.Errorf(codes.Internal, "%s failed for %s: %v", recordAction, u.Username, applyErr)
	}

	byID, byUser := operatorIdentity(ctx)
	rec := home.OperatorGrant{
		Action: recordAction, ByUser: byUser,
		TargetUser: u.Username, At: time.Now(),
	}
	rec.ByUID, rec.BySID = splitIdentity(byID)
	targetUID, targetSID := splitIdentity(u.Uid)
	if targetUID != nil {
		rec.TargetUID = *targetUID
	}
	rec.TargetSID = targetSID
	if err := s.HomeDir.AppendOperatorGrant(rec); err != nil {
		slog.Error("operator "+recordAction+": attribution record not written", "target", u.Username, "err", err)
	}
	mut := &runnyv1.OperatorMutation{User: u.Username, Sid: targetSID}
	if targetUID != nil {
		mut.Uid = *targetUID
	}
	return mut, nil
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
		e := &runnyv1.Operator{User: op.User}
		uid, sid := splitIdentity(op.ID)
		if uid != nil {
			e.Uid = *uid
		}
		e.Sid = sid
		if g := latestGrant(grants, op.ID); g != nil {
			e.GrantedBy = g.ByUser
			e.GrantedAt = timestamppb.New(g.At)
		}
		resp.Operators = append(resp.Operators, e)
	}
	return resp, nil
}

// grantTargetID reads a record's target identity in platform-native form,
// whichever field the record carries: target_sid when present, else the
// legacy target_uid rendered back to its decimal string. Old darwin records
// (written before target_sid existed) and new ones compare identically.
func grantTargetID(g *home.OperatorGrant) string {
	if g.TargetSID != "" {
		return g.TargetSID
	}
	return strconv.FormatUint(uint64(g.TargetUID), 10)
}

// latestGrant returns the most recent "grant" record for the identity id,
// or nil if none exists (the install-time bootstrap operator, shown as
// "(install)").
func latestGrant(grants []home.OperatorGrant, id string) *home.OperatorGrant {
	var latest *home.OperatorGrant
	for i := range grants {
		g := &grants[i]
		if grantTargetID(g) != id || g.Action != "grant" {
			continue
		}
		if latest == nil || g.At.After(latest.At) {
			latest = g
		}
	}
	return latest
}

// hasID is the one membership predicate Grant and Revoke share: the two
// prechecks must agree on "is this account an operator" or an entry can
// become un-grantable and un-revocable at once.
func hasID(ops []opacl.Operator, id string) bool {
	return slices.ContainsFunc(ops, func(o opacl.Operator) bool { return o.ID == id })
}
