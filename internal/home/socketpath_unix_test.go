//go:build !windows

package home

import (
	"path/filepath"
	"testing"
)

// The socket lives inside the resolved home on unix — there is no separate
// socket location once the home itself resolves.
func TestSocketPathIsInsideResolvedHome(t *testing.T) {
	systemDir := t.TempDir()
	d, err := resolveClient(systemDir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(systemDir, socketName); d.SocketPath() != want {
		t.Errorf("SocketPath = %q, want %q", d.SocketPath(), want)
	}
}
