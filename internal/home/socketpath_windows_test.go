//go:build windows

package home

import "testing"

// On windows the control channel is a named pipe: the system home resolves to
// the fixed PipeName; a per-user home resolves to a distinct, deterministic,
// per-home name so two users' daemons never collide on one pipe.
func TestSocketPathSystemVsPerUser(t *testing.T) {
	if got := Dir(SystemHomeDir).SocketPath(); got != PipeName {
		t.Errorf("system home SocketPath = %q, want %q", got, PipeName)
	}

	alice := Dir(`C:\Users\alice\.runny`).SocketPath()
	bob := Dir(`C:\Users\bob\.runny`).SocketPath()
	if alice == PipeName || bob == PipeName {
		t.Errorf("per-user pipe must differ from the system pipe: alice=%q bob=%q system=%q", alice, bob, PipeName)
	}
	if alice == bob {
		t.Errorf("distinct per-user homes must yield distinct pipe names, both = %q", alice)
	}
	if alice != Dir(`C:\Users\alice\.runny`).SocketPath() {
		t.Error("per-user pipe name must be deterministic for a given home")
	}
}
