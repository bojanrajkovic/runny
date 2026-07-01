//go:build darwin

package opacl

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// testGrantee/testReadOnlyGrantee are system accounts present on every Mac
// that the current (non-root) test user does not already control — the same
// non-root-owner claim the aclprobe spike validated on a real host.
const (
	testGrantee         = "daemon"
	testReadOnlyGrantee = "_www"
)

func requireGrantee(t *testing.T) {
	t.Helper()
	if _, err := user.Lookup(testGrantee); err != nil {
		t.Skipf("test principal %q not present on this host: %v", testGrantee, err)
	}
}

// TestLookupUsernameCapsStuckGoroutines pins the Codex-flagged fix: List
// runs inside socket.mutateOperator while it holds Server.operatorMu, so an
// unbounded NSS lookup here would hang every subsequent grant/revoke, not
// just this call. A second call while the single in-flight slot is occupied
// by a stuck lookup must return "" immediately, not wait out
// usernameLookupBound.
func TestLookupUsernameCapsStuckGoroutines(t *testing.T) {
	release := make(chan struct{})
	orig := userLookupID
	userLookupID = func(string) (*user.User, error) { <-release; return nil, errors.New("unreachable") }
	t.Cleanup(func() {
		userLookupID = orig
		close(release)
	})

	go lookupUsername(1) // occupies the single in-flight slot indefinitely

	deadline := time.Now().Add(2 * time.Second)
	for len(usernameLookupInFlight) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(usernameLookupInFlight) == 0 {
		t.Fatal("first lookupUsername never occupied the in-flight slot")
	}

	start := time.Now()
	name := lookupUsername(2)
	elapsed := time.Since(start)
	if name != "" {
		t.Errorf("lookupUsername while the slot is busy = %q, want empty", name)
	}
	if elapsed > time.Second {
		t.Errorf("call while slot busy took %v, want near-instant (no timeout wait)", elapsed)
	}
}

// TestGrantListRevokeRoundTrip promotes the aclprobe spike's validated
// claims (A: non-root owner sets an inheriting ACE; D1/D2: live grant/revoke
// on an existing node with no restart; E: cgo acl_get_file + mbr_uuid_to_id
// reads the principal back) into a real Go test.
func TestGrantListRevokeRoundTrip(t *testing.T) {
	requireGrantee(t)
	dir := t.TempDir()
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A real socket wouldn't exist yet at Grant time in this isolated test;
	// Grant stamps whatever path it's given directly (mirrors the live
	// socket node, which already exists and so doesn't inherit).
	sock := filepath.Join(homeDir, "runnyd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if err := Grant(ctx, homeDir, sock, testGrantee); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	ops, err := List(homeDir)
	if err != nil {
		t.Fatalf("List after grant: %v", err)
	}
	if !ContainsUser(ops, testGrantee) {
		t.Fatalf("granted operator %q not found: %+v", testGrantee, ops)
	}

	if err := Revoke(ctx, homeDir, sock, testGrantee); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ops, err = List(homeDir)
	if err != nil {
		t.Fatalf("List after revoke: %v", err)
	}
	if ContainsUser(ops, testGrantee) {
		t.Fatalf("revoked operator %q still present: %+v", testGrantee, ops)
	}
}

// TestListUnreadableDirHasNoOperators pins the fail-open contract: a
// directory with no extended ACL (acl_get_file returns NULL) reports no
// operators and no error — ListOperators must not treat "nothing granted
// yet" as a failure.
func TestListUnreadableDirHasNoOperators(t *testing.T) {
	dir := t.TempDir()
	ops, err := List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ops) != 0 {
		t.Errorf("expected no operators on a plain dir, got %+v", ops)
	}
}

// TestListExcludesReadOnlyACE pins the fix for a real lockout bug: a home
// dir carries TWO principal ACEs in production — the operator's (from
// OperatorACE, which includes write) and the _runny service account's (from
// sysdaemon's serviceACE, read-only, no write — it exists so the daemon can
// read operator-landed files, not to make _runny an operator). List must
// only count ACEs that can actually connect() to the socket (the write bit
// — the aclprobe spike's claim C2), or the service account is silently
// counted as a second "operator" and the last-operator revoke guard never
// fires on a real install.
func TestListExcludesReadOnlyACE(t *testing.T) {
	requireGrantee(t)
	if _, err := user.Lookup(testReadOnlyGrantee); err != nil {
		t.Skipf("test principal %q not present on this host: %v", testReadOnlyGrantee, err)
	}
	dir := t.TempDir()
	homeDir := filepath.Join(dir, "home")
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(homeDir, "runnyd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := bounded.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := Grant(ctx, homeDir, sock, testGrantee); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// Stamp the exact read-only ACE serviceACE writes (sysdaemon.go), minus
	// write/append/execute/directory-management — no import of sysdaemon to
	// avoid a package cycle; the literal is pinned by
	// TestACEsPinned in internal/sysdaemon.
	readOnlyACE := "user:" + testReadOnlyGrantee + " allow " +
		"list,search,read,readattr,readextattr,readsecurity,file_inherit,directory_inherit"
	if out, err := exec.CommandContext(ctx, "/bin/chmod", "+a", readOnlyACE, homeDir).CombinedOutput(); err != nil {
		t.Fatalf("stamping the read-only ACE: %v: %s", err, out)
	}

	ops, err := List(homeDir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !ContainsUser(ops, testGrantee) {
		t.Errorf("real operator missing: %+v", ops)
	}
	if ContainsUser(ops, testReadOnlyGrantee) {
		t.Errorf("read-only (no write) ACE counted as an operator: %+v", ops)
	}
}
