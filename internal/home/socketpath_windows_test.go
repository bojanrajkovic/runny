//go:build windows

package home

import "testing"

// On windows the control channel is a named pipe with a fixed, home-independent
// name — SocketPath returns it regardless of which home resolved.
func TestSocketPathIsFixedPipeName(t *testing.T) {
	for _, dir := range []Dir{Dir(`C:\ProgramData\runny`), Dir(`C:\Users\alice\.runny`)} {
		if got := dir.SocketPath(); got != PipeName {
			t.Errorf("Dir(%q).SocketPath() = %q, want %q", dir, got, PipeName)
		}
	}
}
