package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The system daemon (run as the service account that OWNS the installer-created
// SystemHomeDir) binds/writes there; everyone else falls back to the per-user
// ~/.runny. The server keys on OWNERSHIP, not writability: the operator account
// is granted dir-write via an ACL entry on the directory (to land the App key and edit
// config), so a writability test would also pass for an operator running runnyd
// by hand — and a per-user daemon must never bind the system socket. A
// t.TempDir() is owned by the test process, so it stands in for the owned home.
func TestResolveServerPrefersOwnedSystemDir(t *testing.T) {
	systemDir := t.TempDir() // owned by this process
	got, err := resolveServer(systemDir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != systemDir {
		t.Errorf("resolveServer(owned) = %q, want %q", got, systemDir)
	}
}

func TestResolveServerFallsBackWhenSystemDirAbsent(t *testing.T) {
	got, err := resolveServer(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(string(got)), "/.runny") {
		t.Errorf("resolveServer(absent) = %q, want a per-user ~/.runny", got)
	}
}

// A present-but-not-owned system dir must fall back — this is the case the
// operator's ACL dir-write created, where writability alone would wrongly
// resolve to the system home. A non-root test can't chown a dir to a foreign
// uid, so it leans on /usr: root-owned on both darwin and linux, and not owned
// by a non-root test process.
func TestResolveServerFallsBackWhenSystemDirNotOwned(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root owns everything; the not-owned path is unreachable")
	}
	got, err := resolveServer("/usr")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(string(got)), "/.runny") {
		t.Errorf("resolveServer(not-owned) = %q, want per-user fallback", got)
	}
}

// The client keys on EXISTENCE — the operator reaches the dir via an inheriting
// ACL granting read/traverse, not necessarily dir-write — so a present system
// home wins, else the per-user one.
func TestResolveClientPrefersExistingSystemDir(t *testing.T) {
	systemDir := t.TempDir()
	got, err := resolveClient(systemDir)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != systemDir {
		t.Errorf("resolveClient(existing) = %q, want %q", got, systemDir)
	}
}

func TestResolveClientFallsBackWhenSystemDirAbsent(t *testing.T) {
	got, err := resolveClient(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(string(got)), "/.runny") {
		t.Errorf("resolveClient(absent) = %q, want per-user ~/.runny", got)
	}
}
