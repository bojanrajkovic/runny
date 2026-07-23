//go:build windows

package home

import (
	"crypto/sha256"
	"encoding/hex"

	"golang.org/x/sys/windows"
)

// SystemHomeDir is the fixed home of a non-root system daemon; see the unix
// declaration for the deployment model it anchors. The conventional
// %ProgramData% path is hardcoded — honoring a relocated %ProgramData% is out
// of scope.
const SystemHomeDir = `C:\ProgramData\runny`

// PipeName is the system daemon's control-channel pipe — a single fixed name
// its clients (the elevated operator's runnyctl, running as a different
// account) can compute without knowing the daemon's identity. runnyd binds it
// and clients dial it.
const PipeName = `\\.\pipe\runnyd`

// SocketPath returns the windows control channel: a named pipe (not an in-home
// file — a pipe lives in the kernel's pipe namespace). The system daemon uses
// the fixed PipeName; a per-user daemon derives a name from its resolved home
// so two users' daemons never collide on one pipe (and cannot squat each
// other's — pipe instances are first-come). A per-user client resolves the same
// home and so computes the same name. Owner-only access is the pipe's SD, set
// at bind (see internal/socket's listen), the pipe-namespace analogue of
// darwin's 0600 socket.
func (d Dir) SocketPath() string {
	if string(d) == SystemHomeDir {
		return PipeName
	}
	sum := sha256.Sum256([]byte(d))
	return `\\.\pipe\runnyd-` + hex.EncodeToString(sum[:8])
}

// ownedByCurrentUser reports whether path exists and its owner SID equals the
// current process token's user SID — the same ownership (not writability)
// signal ResolveServer keys on.
func ownedByCurrentUser(path string) bool {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	return owner.Equals(tu.User.Sid)
}
