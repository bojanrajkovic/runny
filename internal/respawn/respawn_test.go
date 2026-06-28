package respawn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"howett.net/plist"

	"github.com/bojanrajkovic/runny/internal/home"
)

// writePlist writes a minimal LaunchDaemon plist whose ProgramArguments[0] is
// program, returning its path.
func writePlist(t *testing.T, dir string, program ...string) string {
	t.Helper()
	data, err := plist.Marshal(struct {
		ProgramArguments []string `plist:"ProgramArguments"`
	}{program}, plist.XMLFormat)
	if err != nil {
		t.Fatalf("marshal plist: %v", err)
	}
	p := filepath.Join(dir, "daemon.plist")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("write plist: %v", err)
	}
	return p
}

// echoRunner reports back the path it was asked to exec as the "version", so a
// test can assert WHICH binary would be read — the os.Executable pitfall is that
// the running (old) path is wrong; the resolved (new) target is right.
func echoRunner(_ context.Context, path string) (string, error) { return "v=" + path, nil }

// The load-bearing test: a brew upgrade repoints the opt-symlink to a new binary
// while the plist still names the symlink. Resolving the plist Program path must
// follow the symlink to its CURRENT target — never read os.Executable, which on
// the running process still points at the old started-from path.
func TestResolvesRepointedSymlink(t *testing.T) {
	dir := t.TempDir()
	oldBin := filepath.Join(dir, "old-runnyd")
	newBin := filepath.Join(dir, "new-runnyd")
	for _, b := range []string{oldBin, newBin} {
		if err := os.WriteFile(b, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "runnyd") // the stable opt-symlink
	if err := os.Symlink(oldBin, link); err != nil {
		t.Fatal(err)
	}
	plistPath := writePlist(t, dir, link)

	// brew upgrade: repoint the symlink at the new binary.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(newBin, link); err != nil {
		t.Fatal(err)
	}

	got, ok := targetVersion(context.Background(), plistPath, echoRunner)
	if !ok {
		t.Fatal("ok=false, want a resolved version")
	}
	wantNew, _ := filepath.EvalSymlinks(newBin)
	if got != "v="+wantNew {
		t.Errorf("resolved %q, want the NEW target %q — symlink not re-resolved", got, "v="+wantNew)
	}
}

func TestTargetVersionQuietCases(t *testing.T) {
	dir := t.TempDir()
	realBin := filepath.Join(dir, "runnyd")
	if err := os.WriteFile(realBin, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	boom := func(_ context.Context, _ string) (string, error) { return "", errors.New("exec failed") }
	// ExecRunner trims, so a binary that prints only whitespace surfaces as "".
	empty := func(_ context.Context, _ string) (string, error) { return "", nil }

	cases := []struct {
		name      string
		plistPath string
		run       Runner
	}{
		{"missing plist", filepath.Join(dir, "absent.plist"), echoRunner},
		{"empty ProgramArguments", writePlist(t, t.TempDir()), echoRunner},
		{"program does not exist", writePlist(t, t.TempDir(), filepath.Join(dir, "nope")), echoRunner},
		{"exec error", writePlist(t, t.TempDir(), realBin), boom},
		{"empty version output", writePlist(t, t.TempDir(), realBin), empty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if v, ok := targetVersion(context.Background(), tc.plistPath, tc.run); ok {
				t.Errorf("ok=true (%q), want quiet (ok=false)", v)
			}
		})
	}
}

// A daemon that is not the system daemon (a per-user agent at ~/.runny) has no
// LaunchDaemon plist to read — out of scope, so it stays quiet without touching
// the filesystem.
func TestTargetVersionPerUserQuiet(t *testing.T) {
	if v, ok := TargetVersion(context.Background(), home.Dir("/Users/someone/.runny")); ok {
		t.Errorf("per-user home returned ok=true (%q), want quiet", v)
	}
}
