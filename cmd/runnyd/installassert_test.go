package main

import (
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func TestSystemHomeOwnershipError(t *testing.T) {
	sys := home.Dir(home.SystemHomeDir)
	perUser := home.Dir("/Users/op/.runny")
	cases := []struct {
		name           string
		dir            home.Dir
		managedService bool
		exists         bool
		wantErr        bool
	}{
		// The broken install: the service manager started us, and we fell back
		// to a per-user home because we do not own the (existing) system home.
		// Previously expressed as a uid range, which no windows service could
		// ever satisfy -- os.Geteuid() is -1 there, so the guard was dead on
		// the platform whose failure mode is hardest to read (the service shows
		// Running while runnyctl reports the daemon unreachable, because the
		// two are looking at different pipes).
		{"managed service, doesn't own existing system home", perUser, true, true, true},
		{"managed service, owns the system home", sys, true, true, false},
		{"managed service, no system install present", perUser, true, false, false},
		// A human running a per-user daemon beside a system install is a
		// supported shape, not a misconfiguration -- this is why ownership
		// alone cannot drive the guard.
		{"login user falls back legitimately", perUser, false, true, false},
	}
	for _, tc := range cases {
		err := systemHomeOwnershipError(tc.dir, tc.managedService, tc.exists)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
