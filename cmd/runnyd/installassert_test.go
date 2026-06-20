package main

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestSystemHomeOwnershipError(t *testing.T) {
	sys := home.Dir(home.SystemHomeDir)
	perUser := home.Dir("/Users/op/.runny")
	cases := []struct {
		name    string
		dir     home.Dir
		euid    int
		exists  bool
		wantErr bool
	}{
		// The broken install: a service-range uid that fell back to a per-user
		// home because it doesn't own the (existing) system home.
		{"service uid, doesn't own existing system home", perUser, 250, true, true},
		{"service uid, owns the system home", sys, 250, true, false},
		{"service uid, no system install present", perUser, 250, false, false},
		// A login user running a per-user agent beside a system install is fine.
		{"login user falls back legitimately", perUser, 501, true, false},
		{"root is excluded (not the deployment model)", perUser, 0, true, false},
	}
	for _, tc := range cases {
		err := systemHomeOwnershipError(tc.dir, tc.euid, tc.exists)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
