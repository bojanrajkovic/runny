//go:build darwin

package opacl

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// testGrantee is a system account present on every Mac that the current
// (non-root) test user does not already control — the same non-root-owner
// claim the aclprobe spike validated on a real host.
const testGrantee = "daemon"

func requireGrantee(t *testing.T) {
	t.Helper()
	if _, err := user.Lookup(testGrantee); err != nil {
		t.Skipf("test principal %q not present on this host: %v", testGrantee, err)
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
	if !hasOperator(ops, testGrantee) {
		t.Fatalf("granted operator %q not found: %+v", testGrantee, ops)
	}

	if err := Revoke(ctx, homeDir, sock, testGrantee); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	ops, err = List(homeDir)
	if err != nil {
		t.Fatalf("List after revoke: %v", err)
	}
	if hasOperator(ops, testGrantee) {
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

func hasOperator(ops []Operator, username string) bool {
	for _, op := range ops {
		if op.User == username {
			return true
		}
	}
	return false
}
