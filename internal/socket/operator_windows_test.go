//go:build windows

package socket

import (
	"os/user"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRefuseGrantTargetWindows pins the Windows grant-target rule, which must
// mirror opacl.operatorSIDs exactly (no write-granted-yet-read-hidden gap):
// SYSTEM (well-known), Administrators (alias), and a service SID (S-1-5-80-*)
// are all refused — the last is the one LookupAccountSid mislabels as
// SidTypeUser in the daemon's context, so the structural exclusion, not the
// type check, is what catches it — while the test process's own account (a
// real user) passes.
func TestRefuseGrantTargetWindows(t *testing.T) {
	for _, c := range []struct{ name, sid string }{
		{"SYSTEM", "S-1-5-18"},
		{"Administrators", "S-1-5-32-544"},
		{"service SID", "S-1-5-80-3139157870-2983391045-3678747466-658725712-1809340420"},
	} {
		err := refuseGrantTarget(&user.User{Uid: c.sid, Username: c.name})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("refuseGrantTarget(%s) = %v, want InvalidArgument", c.name, err)
		}
	}

	me, err := user.Current()
	if err != nil {
		t.Skipf("user.Current unavailable: %v", err)
	}
	if err := refuseGrantTarget(me); err != nil {
		t.Errorf("refuseGrantTarget(current user) = %v, want nil", err)
	}

	if err := refuseGrantTarget(&user.User{Uid: "not-a-sid", Username: "junk"}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("refuseGrantTarget(unparseable sid) = %v, want InvalidArgument", err)
	}
}
