//go:build windows

package socket

import (
	"os/user"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestRefuseGrantTargetWindows pins the Windows grant-target rule: only
// SidTypeUser accounts are grantable. SYSTEM (a well-known-group SID) and
// Administrators (an alias SID) must be refused — they bypass the home DACL
// anyway — while the test process's own account (a real user) passes.
func TestRefuseGrantTargetWindows(t *testing.T) {
	for _, c := range []struct{ name, sid string }{
		{"SYSTEM", "S-1-5-18"},
		{"Administrators", "S-1-5-32-544"},
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
