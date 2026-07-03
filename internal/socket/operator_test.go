//go:build darwin

package socket

import (
	"os"
	"os/user"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/opacl"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// testGrantee1/2 are system accounts present on every Mac that the test
// user does not already control — the same principals internal/opacl's own
// tests grant against.
const (
	testGrantee1 = "daemon"
	testGrantee2 = "_www"
)

func requireGrantees(t *testing.T) {
	t.Helper()
	for _, name := range []string{testGrantee1, testGrantee2} {
		if _, err := resolveOperatorAccount(name); err != nil {
			t.Skipf("test principal %q not present on this host: %v", name, err)
		}
	}
}

// newOperatorTestServer builds a *Server rooted at a fresh temp home with a
// stand-in socket file already present (mirroring the live socket node
// Grant/Revoke stamp directly). HomeDir deliberately does NOT equal
// home.SystemHomeDir — tests that need to exercise the gated logic call the
// unexported grantOperator/revokeOperator directly instead of the RPC
// wrapper, which is exactly what production code cannot do (by design).
func newOperatorTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(homeDir, "runnyd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s := &Server{HomeDir: home.Dir(homeDir)}
	s.socketPath = sock
	return s
}

func TestGrantOperatorRequiresSystemDaemon(t *testing.T) {
	s := newOperatorTestServer(t) // HomeDir != home.SystemHomeDir
	_, err := s.GrantOperator(t.Context(), &runnyv1.GrantOperatorRequest{User: testGrantee1})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestRevokeOperatorRequiresSystemDaemon(t *testing.T) {
	s := newOperatorTestServer(t)
	_, err := s.RevokeOperator(t.Context(), &runnyv1.RevokeOperatorRequest{User: testGrantee1})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

// TestResolveOperatorAccountByName pins the name-lookup fast path.
func TestResolveOperatorAccountByName(t *testing.T) {
	requireGrantees(t)
	u, err := resolveOperatorAccount(testGrantee1)
	if err != nil {
		t.Fatalf("resolveOperatorAccount(%q): %v", testGrantee1, err)
	}
	if u.Username != testGrantee1 {
		t.Errorf("Username = %q, want %q", u.Username, testGrantee1)
	}
}

func TestGrantOperatorRefusesRoot(t *testing.T) {
	s := newOperatorTestServer(t)
	_, err := s.grantOperator(t.Context(), "root")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGrantOperatorRefusesUnknownUser(t *testing.T) {
	s := newOperatorTestServer(t)
	_, err := s.grantOperator(t.Context(), "no-such-runny-test-user-xyz")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestGrantOperatorGrantsAndRecords(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	mut, err := s.grantOperator(ctx, testGrantee1)
	if err != nil {
		t.Fatalf("grantOperator: %v", err)
	}
	if mut.GetUser() != testGrantee1 {
		t.Errorf("OperatorMutation.User = %q, want %q", mut.GetUser(), testGrantee1)
	}

	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		t.Fatalf("opacl.List: %v", err)
	}
	if !opacl.ContainsUser(ops, testGrantee1) {
		t.Fatalf("granted operator missing from ACL: %+v", ops)
	}

	grants, err := s.HomeDir.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(grants) != 1 || grants[0].Action != "grant" || grants[0].TargetUser != testGrantee1 ||
		grants[0].ByUID == nil || *grants[0].ByUID != 501 {
		t.Errorf("grant record wrong: %+v", grants)
	}
}

// TestGrantRecordUnknownPeerCredIsNotRoot pins that a grant whose peer cred
// could not be read appends a record with by_uid absent — never uid 0, which
// would read as root granted it in the audit trail.
func TestGrantRecordUnknownPeerCredIsNotRoot(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	if _, err := s.grantOperator(t.Context(), testGrantee1); err != nil {
		t.Fatalf("grantOperator: %v", err)
	}
	grants, err := s.HomeDir.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("expected 1 record, got %+v", grants)
	}
	if grants[0].ByUID != nil {
		t.Errorf("unknown peer cred recorded by_uid=%d, want absent", *grants[0].ByUID)
	}
	if grants[0].ByUser != "" {
		t.Errorf("unknown peer cred recorded by_user=%q, want empty", grants[0].ByUser)
	}
}

func TestGrantOperatorRefusesAlreadyOperator(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("first grant: %v", err)
	}
	_, err := s.grantOperator(ctx, testGrantee1)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("second grant: code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestRevokeOperatorRefusesLastOperator(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, err := s.revokeOperator(ctx, testGrantee1)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestRevokeOperatorRefusesNonOperator(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	// Grant one so this isn't ALSO the last-operator case.
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("grant: %v", err)
	}
	_, err := s.revokeOperator(ctx, testGrantee2)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

func TestRevokeOperatorRevokesAndRecords(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("grant 1: %v", err)
	}
	if _, err := s.grantOperator(ctx, testGrantee2); err != nil {
		t.Fatalf("grant 2: %v", err)
	}
	if _, err := s.revokeOperator(ctx, testGrantee2); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		t.Fatalf("opacl.List: %v", err)
	}
	if opacl.ContainsUser(ops, testGrantee2) {
		t.Errorf("revoked operator still present: %+v", ops)
	}
	if !opacl.ContainsUser(ops, testGrantee1) {
		t.Errorf("unrelated operator was removed: %+v", ops)
	}

	grants, err := s.HomeDir.ReadOperatorGrants()
	if err != nil {
		t.Fatalf("ReadOperatorGrants: %v", err)
	}
	var sawRevoke bool
	for _, g := range grants {
		if g.Action == "revoke" && g.TargetUser == testGrantee2 {
			sawRevoke = true
		}
	}
	if !sawRevoke {
		t.Errorf("no revoke record for %s: %+v", testGrantee2, grants)
	}
}

// revokePrecheckForTest duplicates revokeOperator's precheck closure so this
// test can drive mutateOperator directly with a slowed-down apply step,
// widening the List-then-mutate race window deterministically instead of
// hoping real scheduling happens to interleave two goroutines badly.
func revokePrecheckForTest(ops []opacl.Operator, uid uint32, u *user.User) error {
	if !opacl.ContainsUID(ops, uid) {
		return status.Errorf(codes.FailedPrecondition, "%s is not an operator", u.Username)
	}
	if len(ops) <= 1 {
		return status.Error(codes.FailedPrecondition,
			"refusing to revoke the last operator — recover with: sudo runnyctl install-daemon")
	}
	return nil
}

// TestMutateOperatorSerializesConcurrentRevokes pins the code-review fix
// for a real TOCTOU race: mutateOperator's List->precheck->apply sequence
// had no lock, so two concurrent revokes of the last two operators could
// both read the pre-mutation list, both pass the last-operator guard, and
// both succeed — zero operators left, recoverable only via install-daemon.
// Goroutine 1's apply is slowed so it's still holding the lock (and hasn't
// mutated the ACL yet) when goroutine 2 starts; if mutateOperator
// serializes correctly, goroutine 2 blocks until goroutine 1 finishes and
// then correctly sees only one operator left.
func TestMutateOperatorSerializesConcurrentRevokes(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("grant 1: %v", err)
	}
	if _, err := s.grantOperator(ctx, testGrantee2); err != nil {
		t.Fatalf("grant 2: %v", err)
	}

	slowRevoke := func(actx bounded.Context, homeDir, sock, username string) error {
		time.Sleep(200 * time.Millisecond)
		return opacl.Revoke(actx, homeDir, sock, username)
	}

	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err1 = s.mutateOperator(ctx, testGrantee1, "revoke", revokePrecheckForTest, slowRevoke)
	}()
	time.Sleep(50 * time.Millisecond) // let goroutine 1 acquire the lock and enter its slow apply
	_, err2 = s.mutateOperator(ctx, testGrantee2, "revoke", revokePrecheckForTest, opacl.Revoke)
	wg.Wait()

	if err1 == nil && err2 == nil {
		t.Fatal("both concurrent revokes succeeded — the last-operator guard was bypassed by the race")
	}
	ops, err := opacl.List(s.HomeDir.String())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ops) < 1 {
		t.Fatalf("zero operators remain after concurrent revokes: err1=%v err2=%v, ops=%+v", err1, err2, ops)
	}
}

// TestListOperatorsJoinsAttribution pins the design's join contract: an
// account granted through the RPC shows its granter and timestamp; the
// install-bootstrap case (an ACL entry with no grant record) is exercised
// separately since it needs an ACE stamped outside the RPC path.
func TestListOperatorsJoinsAttribution(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	ctx := asOperator(t.Context(), 501)
	if _, err := s.grantOperator(ctx, testGrantee1); err != nil {
		t.Fatalf("grant: %v", err)
	}

	resp, err := s.ListOperators(t.Context(), &runnyv1.ListOperatorsRequest{})
	if err != nil {
		t.Fatalf("ListOperators: %v", err)
	}
	found := false
	for _, op := range resp.GetOperators() {
		if op.GetUser() != testGrantee1 {
			continue
		}
		found = true
		if op.GetGrantedAt() == nil {
			t.Error("GrantedAt unset for an RPC-granted operator")
		}
	}
	if !found {
		t.Fatalf("granted operator missing from ListOperators: %+v", resp.GetOperators())
	}
}

// TestListOperatorsBootstrapHasNoAttribution pins the "(install)" case: an
// ACL entry with no operator-grants.jsonl record (the install-time
// bootstrap operator) lists with an empty GrantedBy/unset GrantedAt.
func TestListOperatorsBootstrapHasNoAttribution(t *testing.T) {
	requireGrantees(t)
	s := newOperatorTestServer(t)
	bctx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// Stamp the ACE directly via opacl, bypassing the RPC — simulating the
	// install-time bootstrap operator, which has no grants.jsonl record.
	if err := opacl.Grant(bctx, s.HomeDir.String(), s.socketPath, testGrantee1); err != nil {
		t.Fatalf("opacl.Grant: %v", err)
	}

	resp, err := s.ListOperators(t.Context(), &runnyv1.ListOperatorsRequest{})
	if err != nil {
		t.Fatalf("ListOperators: %v", err)
	}
	found := false
	for _, op := range resp.GetOperators() {
		if op.GetUser() != testGrantee1 {
			continue
		}
		found = true
		if op.GetGrantedBy() != "" || op.GetGrantedAt() != nil {
			t.Errorf("bootstrap operator should have no attribution: %+v", op)
		}
	}
	if !found {
		t.Fatalf("stamped operator missing from ListOperators: %+v", resp.GetOperators())
	}
}
