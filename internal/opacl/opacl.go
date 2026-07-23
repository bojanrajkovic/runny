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

// OperatorACE grants the named account full directory management plus
// read/write on inherited files: edit config, atomically rename over it,
// land the App key, and read the daemon's artifacts. It overrides the
// home's 0700 POSIX mode (ACL allow is evaluated ahead of POSIX). Pinned
// literal — validated by the PR4c spike (internal/sysdaemon) and, for the
// live-grant path, the aclprobe spike.
func OperatorACE(operator string) string {
	return "user:" + operator + " allow " +
		"list,add_file,search,delete,add_subdirectory,delete_child," +
		"readattr,writeattr,readextattr,writeextattr,readsecurity," +
		"read,write,append,execute," + ACLInherit
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
