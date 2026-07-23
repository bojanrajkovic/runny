//go:build windows

package socket

import (
	"os/user"

	"golang.org/x/sys/windows"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refuseGrantTarget rejects accounts that must never receive an operator
// ACE. On Windows only real user accounts (SidTypeUser) are grantable: the
// membership rule the DACL reader applies excludes everything else, so an
// ACE for SYSTEM, Administrators, a group, or a service SID would either be
// invisible to the operator list or a lie in it — and SYSTEM plus elevated
// Administrators already bypass the home DACL (Full ACEs from the install
// bootstrap, plus SeTakeOwnershipPrivilege), the same already-privileged
// rationale behind darwin's root refusal.
func refuseGrantTarget(u *user.User) error {
	sid, err := windows.StringToSid(u.Uid)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "account %s has an unparseable SID %q: %v", u.Username, u.Uid, err)
	}
	_, _, accType, err := sid.LookupAccount("")
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "resolving %s's SID %s: %v", u.Username, u.Uid, err)
	}
	if accType != windows.SidTypeUser {
		return status.Errorf(codes.InvalidArgument,
			"refusing to grant %s: not a regular user account (SYSTEM, Administrators, groups, and service principals cannot be operators)", u.Username)
	}
	return nil
}
