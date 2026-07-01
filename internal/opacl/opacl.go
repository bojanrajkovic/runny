// Package opacl grants, revokes, and reads the operator accounts on a runny
// system daemon's home directory ACL — the same mechanism install-daemon
// uses to bootstrap the first operator, now exposed as a live daemon RPC so
// an existing operator can grant another with no root and no restart.
package opacl

// Operator is one account found in a home directory's operator ACL.
type Operator struct {
	UID  uint32
	User string
}

// ContainsUID reports whether ops includes uid.
func ContainsUID(ops []Operator, uid uint32) bool {
	for _, op := range ops {
		if op.UID == uid {
			return true
		}
	}
	return false
}

// ContainsUser reports whether ops includes username.
func ContainsUser(ops []Operator, username string) bool {
	for _, op := range ops {
		if op.User == username {
			return true
		}
	}
	return false
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
