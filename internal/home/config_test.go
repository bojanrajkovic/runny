package home

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimalConfig = `
pools:
  - name: mac
    os: darwin
    image: ghcr.io/cirruslabs/macos-tahoe-xcode:26.3
    count: 2
    target:
      owner: bojanrajkovic
      repo: mcp-paprika
    github:
      app_id: 123456
      private_key_path: /tmp/key.pem
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
	p := c.Pools[0]
	if p.RunnerGroupID != 1 || p.SSHUser != "admin" || p.SSHPassword != "admin" {
		t.Errorf("pool defaults: %+v", p)
	}
	if len(p.Labels) != 3 || p.Labels[1] != "macOS" {
		t.Errorf("darwin label default: %v", p.Labels)
	}
	if got := c.Deadlines.AwaitSSH.D(); got != 90*time.Second {
		t.Errorf("AwaitSSH = %v, want 90s", got)
	}
	if got := c.Deadlines.Resolve.D(); got != 60*time.Second {
		t.Errorf("Resolve = %v, want 60s", got)
	}
	if got := p.SSHTimeout.D(); got != 3*time.Second {
		t.Errorf("SSHTimeout = %v, want 3s", got)
	}
	// Hardening defaults ON: the password is a bootstrap credential, not the
	// cycle's (ADR-0013). Opting out is explicit.
	if p.SSHHardening != SSHHardeningRotate {
		t.Errorf("SSHHardening = %q, want %q", p.SSHHardening, SSHHardeningRotate)
	}
	if got := c.Deadlines.SecureSSH.D(); got != 15*time.Second {
		t.Errorf("SecureSSH = %v, want 15s", got)
	}
	if got := c.Limits.MaxDebugHold.D(); got != 2*time.Hour {
		t.Errorf("MaxDebugHold = %v, want 2h", got)
	}
	if p.Target.IsOrg() {
		t.Error("owner/repo target misread as org")
	}
}

func TestLoadConfigMixedPools(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, minimalConfig+`  - name: lin
    os: linux
    image: ghcr.io/cirruslabs/ubuntu:latest
    count: 3
    cpu_cores: 6
    ram_gb: 12
    ssh_hardening: off
    target:
      org: example-org
    github:
      app_id: 654321
      private_key_path: /tmp/lin-key.pem
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(c.Pools) != 2 {
		t.Fatalf("pools = %d", len(c.Pools))
	}
	lin := c.Pools[1]
	if !lin.Target.IsOrg() || lin.Target.String() != "org:example-org" {
		t.Errorf("org target: %+v", lin.Target)
	}
	if lin.Labels[1] != "Linux" {
		t.Errorf("linux label default: %v", lin.Labels)
	}
	// Each pool carries its own App; the two must not be conflated.
	if c.Pools[0].GitHub.AppID != 123456 || lin.GitHub.AppID != 654321 {
		t.Errorf("per-pool app ids: mac=%d lin=%d", c.Pools[0].GitHub.AppID, lin.GitHub.AppID)
	}
	if lin.GitHub.APIBase != "https://api.github.com" {
		t.Errorf("api_base default not applied per pool: %q", lin.GitHub.APIBase)
	}
	// The opt-out must survive YAML 1.1's `off`→boolean trap unquoted — that
	// is the form operators will write.
	if lin.SSHHardening != SSHHardeningOff {
		t.Errorf("SSHHardening = %q, want %q", lin.SSHHardening, SSHHardeningOff)
	}
	if c.Pools[0].SSHHardening != SSHHardeningRotate {
		t.Errorf("default pool hardening = %q, want rotate", c.Pools[0].SSHHardening)
	}
	// Hardware sizing overrides parse; the mac pool left them unset (zero =
	// use the image's baked value).
	if lin.CPUCores != 6 || lin.RAMGB != 12 {
		t.Errorf("sizing override: cpu=%d ram=%d", lin.CPUCores, lin.RAMGB)
	}
	if c.Pools[0].CPUCores != 0 || c.Pools[0].RAMGB != 0 {
		t.Errorf("unset sizing should stay zero: cpu=%d ram=%d", c.Pools[0].CPUCores, c.Pools[0].RAMGB)
	}
}

func TestLoadConfigValidation(t *testing.T) {
	cases := []struct {
		name, yaml, wantErr string
	}{
		{"no pools", "retention:\n  cycles_per_slot: 5\n", "at least one pool"},
		{"pool missing github", "pools:\n  - name: x\n    os: linux\n    image: i\n    target: {org: a}\n", "github.app_id is required"},
		{"bad os", minimalConfig + "  - name: w\n    os: windows\n    image: x\n    target: {org: a}\n", "os must be darwin or linux"},
		{"both targets", minimalConfig + "  - name: b\n    os: linux\n    image: x\n    target: {org: a, owner: b, repo: c}\n", "not both"},
		{"half repo target", minimalConfig + "  - name: h\n    os: linux\n    image: x\n    target: {owner: b}\n", "org, or both owner and repo"},
		{"dup names", minimalConfig + "  - name: mac\n    os: linux\n    image: x\n    target: {org: a}\n", "duplicate pool name"},
		{"bad name", minimalConfig + "  - name: Mac_1\n    os: linux\n    image: x\n    target: {org: a}\n", "lowercase"},
		{"negative deadline", minimalConfig + "deadlines:\n  pull_stall: -30s\n", "must be positive"},
		{"negative limit", minimalConfig + "limits:\n  max_idle: -1h\n", "must be positive"},
		{"negative ssh timeout", strings.Replace(minimalConfig, "count: 2", "count: 2\n    ssh_timeout: -3s", 1), "ssh_timeout must be positive"},
		{"bad ssh hardening", strings.Replace(minimalConfig, "count: 2", "count: 2\n    ssh_hardening: maybe", 1), `ssh_hardening must be "rotate" or "off"`},
		{"negative secure_ssh", minimalConfig + "deadlines:\n  secure_ssh: -15s\n", "secure_ssh must be positive"},
		{"negative max_debug_hold", minimalConfig + "limits:\n  max_debug_hold: -1h\n", "max_debug_hold must be positive"},
	}
	for _, tc := range cases {
		_, err := LoadConfig(writeConfig(t, tc.yaml))
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.wantErr)
		}
	}
}

func TestLoadConfigRejectsUnknownKeys(t *testing.T) {
	if _, err := LoadConfig(writeConfig(t, minimalConfig+"runers:\n  count: 3\n")); err == nil {
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
	if got := d.VMDir("mac-1"); got != "/h/.runny/vms/mac-1" {
		t.Errorf("VMDir = %q", got)
	}
	if got := d.ImageBundleDir("ghcr.io/cirruslabs/macos-tahoe-xcode:26.3", "sha256:abc"); got != "/h/.runny/images/ghcr.io_cirruslabs_macos-tahoe-xcode/sha256-abc" {
		t.Errorf("ImageBundleDir = %q", got)
	}
}

// The home is fixed at ~/.runny derived from $HOME: a set RUNNY_HOME must be
// ignored (the inverse of the removed override), not honored.
func TestResolveIgnoresEnvAndIsHomeRooted(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	t.Setenv("RUNNY_HOME", "/custom")
	d, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got, want := d.String(), filepath.Join("/tmp/fakehome", ".runny"); got != want {
		t.Errorf("Resolve() = %q, want %q — RUNNY_HOME must be ignored, the home derived from $HOME", got, want)
	}
}

// launchd can hand an agent a degenerate $HOME; with no override left to correct
// it, Resolve must fail loudly rather than derive a wrong home. The guard is
// structural (IsAbs + non-root Clean), so every spelling of root and any
// relative home is rejected — not just the literal "/".
func TestResolveRejectsDegenerateHome(t *testing.T) {
	for _, h := range []string{"/", "//", "///", "relative/home", "."} {
		t.Run(h, func(t *testing.T) {
			t.Setenv("HOME", h)
			if _, err := Resolve(); err == nil {
				t.Fatalf("Resolve() with $HOME=%q must error, not derive a wrong home", h)
			}
		})
	}
}
