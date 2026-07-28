//go:build !windows

package main

import "testing"

// The launch-context predicate is the load-bearing half of the system-home
// guard: get it wrong and either a service misconfiguration goes unreported,
// or a human running a per-user daemon beside a system install is refused.
// Root is excluded deliberately — not this project's deployment model, and it
// owns everything anyway. sysdaemon allocates service uids in 200-400.
func TestManagedServiceUIDRange(t *testing.T) {
	for _, tc := range []struct {
		euid int
		want bool
	}{
		{0, false},   // root
		{1, true},    // first service uid
		{199, true},  // below sysdaemon's allocation window, still service-range
		{250, true},  // sysdaemon's actual range
		{499, true},  // last service uid
		{500, false}, // the login-user floor itself
		{501, false}, // a login user
	} {
		if got := managedServiceUID(tc.euid); got != tc.want {
			t.Errorf("managedServiceUID(%d) = %v, want %v", tc.euid, got, tc.want)
		}
	}
}
