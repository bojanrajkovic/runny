//go:build windows

package home

import "golang.org/x/sys/windows"

// SystemHomeDir is the fixed home of a non-root system daemon; see the unix
// declaration for the deployment model it anchors. The conventional
// %ProgramData% path is hardcoded — honoring a relocated %ProgramData% is out
// of scope.
const SystemHomeDir = `C:\ProgramData\runny`

// PipeName is the control channel's named-pipe path on windows — a single
// fixed name, not derived from the home dir (a pipe lives in the kernel's pipe
// namespace, not on disk under the home). runnyd binds it and clients dial it.
const PipeName = `\\.\pipe\runnyd`

// SocketPath returns the windows control channel: the named pipe. The path is
// home-independent (unlike darwin's in-home unix socket), so d is ignored.
func (d Dir) SocketPath() string { return PipeName }

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
