package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/socket"
)

var hexSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// LoadConfigSHA hashes the exact bytes it parsed in one read, so the audit
// hash provably describes the validated config. The hash is stable for
// identical bytes, changes when the file changes, and a missing/invalid file
// yields an error and an empty hash (no hash for bytes that never validated).
func TestLoadConfigSHAStableAndHex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, validConfigYAML(t, 1, "original"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, a, err := home.LoadConfigSHA(path)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := home.LoadConfigSHA(path)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("LoadConfigSHA not stable: %q vs %q", a, b)
	}
	if !hexSHA256.MatchString(a) {
		t.Errorf("LoadConfigSHA not 64 lowercase hex chars: %q", a)
	}
	if err := os.WriteFile(path, validConfigYAML(t, 1, "edited comment"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, c, _ := home.LoadConfigSHA(path); c == a {
		t.Error("LoadConfigSHA unchanged after the file changed")
	}
	if _, got, err := home.LoadConfigSHA(filepath.Join(t.TempDir(), "missing.yaml")); err == nil || got != "" {
		t.Errorf("LoadConfigSHA of a missing file = (%q, %v), want empty hash and an error", got, err)
	}
}

// The preflight post-filter (ADR-0014 decision 7): a failing local-network
// becomes a warning (the respawn's cold start cannot fail it and no config
// edit affects it); a failing disk-headroom keeps its refusal slot with the
// measured-with-guests-running annotation; everything else refuses as-is;
// passing checks vanish.
func TestSplitPreflightChecks(t *testing.T) {
	failed, warnings := splitPreflightChecks([]socket.DoctorCheck{
		{Name: "platform", OK: true, Detail: "darwin/arm64"},
		{Name: "local-network", OK: false, Detail: "cannot reach the guest subnet"},
		{Name: "disk-headroom", OK: false, Detail: "12GB free; <30GB risks mid-job disk exhaustion"},
		{Name: "runner-perm:org:example", OK: false, Detail: "401"},
	})
	if len(warnings) != 1 || warnings[0].Name != "local-network" {
		t.Errorf("warnings = %+v, want just local-network", warnings)
	}
	if len(failed) != 2 {
		t.Fatalf("failed = %+v, want disk-headroom + runner-perm", failed)
	}
	byName := map[string]socket.DoctorCheck{}
	for _, c := range failed {
		byName[c.Name] = c
	}
	dh, ok := byName["disk-headroom"]
	if !ok || !strings.Contains(dh.Detail, "the respawn sweeps clones before re-checking") {
		t.Errorf("disk-headroom refusal lost its environment annotation: %+v", dh)
	}
	if !strings.HasPrefix(dh.Detail, "12GB free") {
		t.Errorf("disk-headroom annotation replaced the original detail: %q", dh.Detail)
	}
	if _, ok := byName["runner-perm:org:example"]; !ok {
		t.Errorf("an ordinary failing check was dropped: %+v", failed)
	}

	gotF, gotW := splitPreflightChecks([]socket.DoctorCheck{{Name: "platform", OK: true}})
	if len(gotF) != 0 || len(gotW) != 0 {
		t.Errorf("all-green checks produced refusals/warnings: %v / %v", gotF, gotW)
	}
}

// A config that does not parse refuses at config-parse, before any client
// construction or network check.
func TestPreflightReloadRefusesUnparseableConfig(t *testing.T) {
	dir := home.Dir(t.TempDir())
	path := dir.ConfigPath()
	if err := os.WriteFile(path, []byte("pools: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sha, failed, warnings := preflightReload(context.Background(), dir, path)
	if sha == "" {
		t.Error("sha empty for a readable (if unparseable) file")
	}
	if len(failed) != 1 || failed[0].Name != "config-parse" || failed[0].OK {
		t.Fatalf("failed = %+v, want one config-parse failure", failed)
	}
	if failed[0].Detail == "" {
		t.Error("config-parse failure has no detail for the operator")
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %+v, want none", warnings)
	}
}

// validConfigYAML is a minimal config that passes LoadConfig (these tests
// never construct GitHub clients, so the key path needn't exist).
func validConfigYAML(t *testing.T, count int, comment string) []byte {
	t.Helper()
	return []byte(`# ` + comment + `
pools:
  - name: mac
    os: darwin
    image: ghcr.io/example/img:latest
    count: ` + strconv.Itoa(count) + `
    target:
      org: example
    github:
      app_id: 1
      private_key_path: /nonexistent/key.pem
`)
}

// makeDoctor's config-drift check: green on an identical file (even after
// comment-only edits — comparison is post-defaulting struct equality, not
// a byte hash), FAIL with the reload hint on a semantic change, FAIL with
// the parse error on a broken file. Run with no clients and a cancelled
// context so the network checks fail instantly and only the local checks
// are interrogated.
func TestMakeDoctorConfigDrift(t *testing.T) {
	dir := home.Dir(t.TempDir())
	path := dir.ConfigPath()
	write := func(b []byte) {
		t.Helper()
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(validConfigYAML(t, 1, "original"))
	cfg, err := home.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	drift := func() socket.DoctorCheck {
		t.Helper()
		for _, c := range makeDoctor(dir, path, cfg, nil)(ctx) {
			if c.Name == "config-drift" {
				return c
			}
		}
		t.Fatal("doctor suite has no config-drift check")
		return socket.DoctorCheck{}
	}

	if c := drift(); !c.OK {
		t.Errorf("identical file reported drift: %+v", c)
	}

	// Comment-only edit: same semantics, different bytes — must stay green.
	write(validConfigYAML(t, 1, "reworded comment"))
	if c := drift(); !c.OK {
		t.Errorf("comment-only edit reported drift: %+v", c)
	}

	// Semantic change: FAIL with the reload hint.
	write(validConfigYAML(t, 2, "original"))
	if c := drift(); c.OK || !strings.Contains(c.Detail, "runnyctl reload") {
		t.Errorf("semantic change not flagged with the reload hint: %+v", c)
	}

	// Broken file: FAIL with the parse error.
	write([]byte("pools: [\n"))
	if c := drift(); c.OK || c.Detail == "" {
		t.Errorf("broken file not flagged with the parse error: %+v", c)
	}
}
