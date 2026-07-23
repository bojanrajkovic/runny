//go:build windows

package socket

import (
	"os/user"

	"golang.org/x/sys/windows"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/opacl"
)

// refuseGrantTarget rejects accounts that must never receive an operator ACE,
// enforcing the exact rule the DACL reader (opacl.operatorSIDs) applies, so no
// SID can be write-granted yet read-hidden — a live ACE that ListOperators and
// RevokeOperator could never see. A target is grantable iff it is neither a
// well-known/service principal (opacl.ExcludedSID — structural, because
// LookupAccountSid mislabels the service SID as SidTypeUser in the daemon's own
// context) nor a group/alias (still caught by SidTypeUser: a domain group
// shares the S-1-5-21- prefix with users, so the prefix check alone cannot
// exclude it). SYSTEM and elevated Administrators already bypass the home DACL
// (Full ACEs from the install bootstrap, plus SeTakeOwnershipPrivilege), the
// same already-privileged rationale behind darwin's root refusal.
func refuseGrantTarget(u *user.User) error {
	if opacl.ExcludedSID(u.Uid) {
		return status.Errorf(codes.InvalidArgument,
			"refusing to grant %s: SYSTEM, service accounts, and built-in groups cannot be operators", u.Username)
	}
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
			"refusing to grant %s: not a regular user account (groups cannot be operators)", u.Username)
	}
	return nil
}
