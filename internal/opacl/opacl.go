// Package opacl grants, revokes, and reads the operator accounts on a runny
// system daemon's home directory ACL — the same mechanism install-daemon
// uses to bootstrap the first operator, now exposed as a live daemon RPC so
// an existing operator can grant another with no root and no restart. Two
// implementations share this surface: darwin's extended ACL (chmod +a/-a)
// and Windows' NTFS DACL (icacls); every other platform reports
// ErrUnsupported.
package opacl

import "os/user"

// Operator is one account found in a home directory's operator ACL. ID is
// the platform-native identity string, following os/user.User.Uid's
// convention: a decimal uid on darwin, a SID string on Windows.
type Operator struct {
	ID   string
	User string
}

// ACLInherit makes an ACE apply to a directory AND every file/dir created
// beneath it. Shared by OperatorACE here and the service account's own
// inheriting ACE in internal/sysdaemon.
const ACLInherit = "file_inherit,directory_inherit"

// OperatorACE grants the named account management of the home DIRECTORY, and
// deliberately nothing below it. It overrides the home's 0700 POSIX mode (ACL
// allow is evaluated ahead of POSIX). Pinned literal.
//
// It does two jobs, both of which stop at the directory. It is the
// authorization registry — HasID reads this entry off the home dir to answer
// "is this caller an operator" — and it is the directory access the operator's
// one remaining filesystem job needs: creating a temp file in the home and
// renaming it over config.yaml, which POSIX charges to the DIRECTORY, not to
// the file being replaced.
//
// Deliberately NOT inheriting, which is a DARWIN judgement: inheritance here is
// copy-at-create, so an inheriting entry leaves a copy on every artifact the
// daemon writes and a copy cannot be reached from the home dir — removing this
// entry could not remove them, and a revoked operator would keep write on
// images/, vms/ and cycles/. Windows maintains inheritance in both directions
// and so grants an inheriting entry there instead (opacl_windows.go). What an
// operator needs to READ below the home is served by mode on this platform
// (home.Dir.Ensure) and by that inherited entry on the other. The service account's own entry (internal/sysdaemon's
// serviceACE) still inherits — it is installed once and never revoked, and the
// daemon does need to reach what it writes.
func OperatorACE(operator string) string {
	return "user:" + operator + " allow " +
		"list,add_file,search,delete,add_subdirectory,delete_child," +
		"readattr,writeattr,readextattr,writeextattr,readsecurity," +
		"read,write,append,execute"
}

// List reads homeDir's operator ACL via the per-platform ListIDs, resolving
// each identity to a username best-effort (an unresolvable identity still
// appears, with an empty User). user.LookupId takes the same
// platform-native identity string ListIDs returns — a decimal uid on
// darwin, a SID on Windows — so the resolution loop needs no platform
// switch. Validated against a real chmod +a grant by the aclprobe spike.
func List(homeDir string) ([]Operator, error) {
	ids, err := ListIDs(homeDir)
	if err != nil {
		return nil, err
	}
	ops := make([]Operator, 0, len(ids))
	for _, id := range ids {
		name := ""
		if u, err := user.LookupId(id); err == nil {
			name = u.Username
		}
		ops = append(ops, Operator{ID: id, User: name})
	}
	return ops, nil
}
