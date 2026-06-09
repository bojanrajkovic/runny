package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
github:
  app_id: 2798371
  private_key_path: /tmp/key.pem
  owner: bojanrajkovic
  repo: runny
image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.3
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Runners.Count != 2 {
		t.Errorf("Runners.Count = %d, want 2", c.Runners.Count)
	}
	if c.Runners.NamePrefix != "runny" {
		t.Errorf("NamePrefix = %q, want runny", c.Runners.NamePrefix)
	}
	if got := c.Deadlines.AwaitSSH.D(); got != 90*time.Second {
		t.Errorf("AwaitSSH = %v, want 90s", got)
	}
	if got := c.Limits.BackoffCap.D(); got != 5*time.Minute {
		t.Errorf("BackoffCap = %v, want 5m", got)
	}
	if c.GitHub.RunnerGroupID != 1 {
		t.Errorf("RunnerGroupID = %d, want 1", c.GitHub.RunnerGroupID)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, minimalConfig+`
runners:
  count: 1
  name_prefix: ci
deadlines:
  await_ssh: 45s
limits:
  backoff_cap: 10m
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if c.Runners.Count != 1 || c.Runners.NamePrefix != "ci" {
		t.Errorf("runner overrides not applied: %+v", c.Runners)
	}
	if got := c.Deadlines.AwaitSSH.D(); got != 45*time.Second {
		t.Errorf("AwaitSSH = %v, want 45s", got)
	}
	if got := c.Limits.BackoffCap.D(); got != 10*time.Minute {
		t.Errorf("BackoffCap = %v, want 10m", got)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, "image: x\n"))
	if err == nil {
		t.Fatal("want validation error for missing github config")
	}
	for _, want := range []string{"app_id", "private_key_path", "owner"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing mention of %s", err, want)
		}
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, minimalConfig+"runers:\n  count: 3\n"))
	if err == nil {
		t.Fatal("want error for misspelled key (strict mode)")
	}
}

func TestLoadConfigBadDuration(t *testing.T) {
	_, err := LoadConfig(writeConfig(t, minimalConfig+"deadlines:\n  boot: fast\n"))
	if err == nil || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("want duration parse error, got %v", err)
	}
}

func TestPaths(t *testing.T) {
	d := Dir("/h/.runny")
	if got := d.VMDir("runner-1"); got != "/h/.runny/vms/runner-1" {
		t.Errorf("VMDir = %q", got)
	}
	if got := d.ImageBundleDir("ghcr.io/cirruslabs/macos-tahoe-xcode:26.3", "sha256:abc"); got != "/h/.runny/images/ghcr.io_cirruslabs_macos-tahoe-xcode/sha256-abc" {
		t.Errorf("ImageBundleDir = %q", got)
	}
	if got := d.ImageBundleDir("ghcr.io/foo/bar@sha256:def", "sha256:def"); !strings.HasSuffix(got, "ghcr.io_foo_bar/sha256-def") {
		t.Errorf("digest-ref ImageBundleDir = %q", got)
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("RUNNY_HOME", "/custom")
	d, err := Resolve("")
	if err != nil || d.String() != "/custom" {
		t.Errorf("Resolve env = %q, %v", d, err)
	}
	d, err = Resolve("/flag")
	if err != nil || d.String() != "/flag" {
		t.Errorf("Resolve flag = %q, %v", d, err)
	}
}
