package opacl

import (
	"strings"
	"testing"
)

// TestOperatorACEDoesNotInherit is the whole point of the operator ACL being
// home-dir-only. The entry answers ONE question — is this account an operator,
// which HasID reads off the home dir — and it grants the directory access an
// operator needs to land a file (create a temp file, rename it over
// config.yaml). It must not propagate to anything.
//
// An entry that inherits is a copy of itself on every artifact the daemon ever
// writes, and a copy is not reachable from the home dir: removing the home's
// entry cannot remove them, so a revoke leaves the account's access to images/,
// vms/ and cycles/ intact. Everything an operator reads out of those goes
// through an RPC, so the copies buy nothing and cost the revoke its meaning.
func TestOperatorACEDoesNotInherit(t *testing.T) {
	ace := OperatorACE("alice")
	for _, flag := range []string{"file_inherit", "directory_inherit"} {
		if strings.Contains(ace, flag) {
			t.Errorf("OperatorACE carries %s — the operator entry must not propagate:\n%s", flag, ace)
		}
	}
	// The directory access an operator does need, and which the rename-based
	// write path rests on: create a file in the home, and replace one.
	for _, perm := range []string{"add_file", "delete_child", "search"} {
		if !strings.Contains(ace, perm) {
			t.Errorf("OperatorACE lost %s — an operator could no longer land a config edit:\n%s", perm, ace)
		}
	}
}
