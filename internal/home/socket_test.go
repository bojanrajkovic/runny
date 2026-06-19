package home

import (
	"os"
	"path/filepath"
	"testing"
)

// The system daemon (run as the service account that owns the installer-created
// shared dir) binds the shared socket; everyone else falls back to the per-user
// home socket. The choice is keyed off the dir being present AND writable — no
// flag — so a per-user agent on a host that also has a system install (a
// misconfiguration) can't try to bind a dir it doesn't own.
func TestServerSocketPathPrefersWritableSharedDir(t *testing.T) {
	shared := t.TempDir() // writable by this process
	d := Dir("/Users/someone/.runny")
	want := filepath.Join(shared, "runnyd.sock")
	if got := serverSocketPath(shared, d); got != want {
		t.Errorf("serverSocketPath(writable shared) = %q, want %q", got, want)
	}
}

func TestServerSocketPathFallsBackWhenSharedDirAbsent(t *testing.T) {
	shared := filepath.Join(t.TempDir(), "absent")
	d := Dir("/Users/someone/.runny")
	if got := serverSocketPath(shared, d); got != d.SocketPath() {
		t.Errorf("serverSocketPath(absent shared) = %q, want per-user %q", got, d.SocketPath())
	}
}

func TestServerSocketPathFallsBackWhenSharedDirNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the write-permission check")
	}
	shared := t.TempDir()
	if err := os.Chmod(shared, 0o500); err != nil { // r-x: present but not writable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o700) }) // let TempDir cleanup remove it
	d := Dir("/Users/someone/.runny")
	if got := serverSocketPath(shared, d); got != d.SocketPath() {
		t.Errorf("serverSocketPath(non-writable shared) = %q, want per-user %q", got, d.SocketPath())
	}
}

// Clients connect to the shared socket when it exists, else the per-user one —
// mirroring the server's bind so the operator's CLI/app reach a system daemon
// with no configuration.
func TestClientSocketPathPrefersExistingSharedSocket(t *testing.T) {
	shared := t.TempDir()
	sock := filepath.Join(shared, "runnyd.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil { // stand-in for a bound socket
		t.Fatal(err)
	}
	d := Dir("/Users/someone/.runny")
	if got := clientSocketPath(shared, d); got != sock {
		t.Errorf("clientSocketPath(existing shared socket) = %q, want %q", got, sock)
	}
}

func TestClientSocketPathFallsBackWhenSharedSocketAbsent(t *testing.T) {
	shared := t.TempDir() // dir exists, no socket file in it
	d := Dir("/Users/someone/.runny")
	if got := clientSocketPath(shared, d); got != d.SocketPath() {
		t.Errorf("clientSocketPath(no shared socket) = %q, want per-user %q", got, d.SocketPath())
	}
}
