package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/socket"
)

func writeKey(t *testing.T, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, []byte("key"), perm); err != nil {
		t.Fatal(err)
	}
	// WriteFile honors umask; force the exact mode.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckPrivateKeyPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("group/world permission bits don't exist on NTFS — Stat().Mode().Perm() collapses to 0666/0444 regardless of chmod, so this unix-only check can't be exercised here")
	}
	tight := writeKey(t, 0o600)
	loose := writeKey(t, 0o644)

	t.Run("tight key passes", func(t *testing.T) {
		c := checkPrivateKeyPerms(&home.Config{Pools: []home.PoolConfig{
			{Name: "a", GitHub: home.GitHubConfig{PrivateKeyPath: tight}},
		}})
		if !c.OK {
			t.Errorf("0600 key should pass: %+v", c)
		}
	})

	t.Run("loose key fails, named once, missing skipped", func(t *testing.T) {
		c := checkPrivateKeyPerms(&home.Config{Pools: []home.PoolConfig{
			{Name: "a", GitHub: home.GitHubConfig{PrivateKeyPath: tight}},
			{Name: "b", GitHub: home.GitHubConfig{PrivateKeyPath: loose}},
			{Name: "c", GitHub: home.GitHubConfig{PrivateKeyPath: loose}},                                     // dup path: named once
			{Name: "d", GitHub: home.GitHubConfig{PrivateKeyPath: filepath.Join(t.TempDir(), "missing.pem")}}, // stat error: skipped
		}})
		if c.OK {
			t.Fatalf("0644 key should fail: %+v", c)
		}
		if got := strings.Count(c.Detail, loose); got != 1 {
			t.Errorf("loose key should appear exactly once (deduped), got %d in %q", got, c.Detail)
		}
		if strings.Contains(c.Detail, tight) {
			t.Errorf("tight key should not be flagged: %q", c.Detail)
		}
	})

	// The check is advisory: a loose key must surface in `runnyctl doctor` but
	// NEVER gate daemon startup or reload. Refusing to boot over a hygiene
	// warning would be strictly worse (no daemon), and since the exit gate does
	// not check perms, gating here would let a mid-drain edit be green-lit by
	// the exit gate and then refused by the respawn child.
	t.Run("never gates startup or reload", func(t *testing.T) {
		failing := socket.DoctorCheck{Name: privateKeyPermsCheck, OK: false, Detail: "loose"}
		if got := failedChecks([]socket.DoctorCheck{failing}); len(got) != 0 {
			t.Errorf("private-key-perms must not gate startup, got %+v", got)
		}
		failed, warnings := splitPreflightChecks([]socket.DoctorCheck{failing})
		if len(failed) != 0 {
			t.Errorf("private-key-perms must not fail reload, got %+v", failed)
		}
		if len(warnings) != 1 {
			t.Errorf("private-key-perms should be a reload warning, got %+v", warnings)
		}
	})
}
