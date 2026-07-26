package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/socket"
	"github.com/bojanrajkovic/runny/internal/sysdaemon"
)

// testRSAKeyPath returns the path to a valid PEM-encoded RSA private key. The
// -test-config gate and startup both read + parse the pool's private key (a
// local, startup-blocking check), so a config is only "valid" with a parseable
// key. The key is generated and written ONCE per process to a stable path — a
// stable path matters because configs that differ only in a comment must stay
// struct-equal (the config-drift check), which a per-call temp path would break.
var (
	testKeyOnce sync.Once
	testKeyFile string
)

func testRSAKeyPath(t *testing.T) string {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		f, err := os.CreateTemp("", "runnyd-test-key-*.pem")
		if err != nil {
			panic(err)
		}
		if _, err := f.Write(pemBytes); err != nil {
			panic(err)
		}
		f.Close()
		testKeyFile = f.Name()
	})
	return testKeyFile
}

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

// config-drift is informational and must never block daemon startup: the
// respawn re-reads the file after the vms-sweep window, so a concurrent
// re-template would otherwise crash-loop on a check that does not affect
// whether THIS config runs.
func TestFailedChecksExcludesConfigDrift(t *testing.T) {
	got := failedChecks([]socket.DoctorCheck{
		{Name: "config-drift", OK: false, Detail: "differs from the running config"},
		{Name: "macos-guest-cap", OK: false, Detail: "over cap"},
		{Name: "platform", OK: true},
	})
	if len(got) != 1 || got[0].Name != "macos-guest-cap" {
		t.Errorf("failedChecks = %+v, want only macos-guest-cap (config-drift excluded)", got)
	}
}

// local-network is excluded from the startup gate too: a self-daemonized runnyd
// fails it, but must keep running and surface the cause (loud log + red
// doctor/status) rather than refuse to boot — foreground and launchd starts are
// fine, and a denied daemon should report the cause, not crash-loop.
func TestFailedChecksExcludesLocalNetwork(t *testing.T) {
	got := failedChecks([]socket.DoctorCheck{
		{Name: "local-network", OK: false, Detail: orphanedDenyDetail},
		{Name: "macos-guest-cap", OK: false, Detail: "over cap"},
	})
	if len(got) != 1 || got[0].Name != "macos-guest-cap" {
		t.Errorf("failedChecks = %+v, want only macos-guest-cap (local-network excluded)", got)
	}
}

// competing-registration is excluded from the startup gate: a leftover per-user
// agent (or an inconclusive launchctl probe) must not refuse boot — the daemon
// surfaces it loudly via doctor and the single-instance flock handles the actual
// runtime conflict. Refusing to start would be strictly worse: no daemon at all,
// over a latent condition that only matters once the operator logs into a GUI.
func TestFailedChecksExcludesCompetingRegistration(t *testing.T) {
	got := failedChecks([]socket.DoctorCheck{
		{Name: "competing-registration", OK: false, Detail: "a per-user runnyd agent is registered"},
		{Name: "macos-guest-cap", OK: false, Detail: "over cap"},
	})
	if len(got) != 1 || got[0].Name != "macos-guest-cap" {
		t.Errorf("failedChecks = %+v, want only macos-guest-cap (competing-registration excluded)", got)
	}
}

// The exit gate re-runs this local check, so a mid-drain edit that overflows
// the darwin guest cap holds the drain instead of crash-looping the respawn.
func TestCheckMacOSGuestCap(t *testing.T) {
	over := checkMacOSGuestCap(&home.Config{Pools: []home.PoolConfig{{Name: "mac", OS: "darwin", Count: macOSGuestCap + 1}}})
	if over.OK {
		t.Errorf("guest cap over by one reported OK: %+v", over)
	}
	ok := checkMacOSGuestCap(&home.Config{Pools: []home.PoolConfig{
		{Name: "mac", OS: "darwin", Count: 1},
		{Name: "lin", OS: "linux", Count: 99}, // linux doesn't count toward the cap
	}})
	if !ok.OK {
		t.Errorf("within-cap darwin + linux reported failure: %+v", ok)
	}
}

// The preflight post-filter: a failing local-network
// becomes a warning (the respawn's cold start cannot fail it and no config
// edit affects it); a failing disk-headroom keeps its refusal slot with the
// measured-with-guests-running annotation; everything else refuses as-is;
// passing checks vanish.
func TestSplitPreflightChecks(t *testing.T) {
	failed, warnings := splitPreflightChecks([]socket.DoctorCheck{
		{Name: "platform", OK: true, Detail: "darwin/arm64"},
		{Name: "local-network", OK: false, Detail: "cannot reach the guest subnet"},
		{Name: "competing-registration", OK: false, Detail: "a per-user runnyd agent is registered"},
		{Name: "disk-headroom", OK: false, Detail: "12GB free; <30GB risks mid-job disk exhaustion"},
		{Name: "runner-perm:org:example", OK: false, Detail: "401"},
	})
	// local-network AND competing-registration both become warnings — neither
	// affects whether the new config is valid, so neither must refuse a reload.
	warnNames := map[string]bool{}
	for _, c := range warnings {
		warnNames[c.Name] = true
	}
	if len(warnings) != 2 || !warnNames["local-network"] || !warnNames["competing-registration"] {
		t.Errorf("warnings = %+v, want local-network + competing-registration", warnings)
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

// checkDiskHeadroom must match the pull guard: a host with enough for the
// fixed 30 GiB floor but not for the configured image's uncompressed size
// must FAIL, not pass — the class of silent-failure greenlight the project
// exists to prevent. A host with enough for the image must pass.
func TestCheckDiskHeadroomImageAware(t *testing.T) {
	const GiB = uint64(1 << 30)
	const imageSize = int64(100 << 30) // 100 GiB image, compressed to something smaller on-disk

	// floor is checkDiskHeadroom's own formula, computed here rather than
	// hardcoded — oci.RequiredHeadroom's margin is platform-specific
	// (windows needs roughly a second full-size copy during VHDX
	// conversion; elsewhere just a flat safety margin), so a literal
	// expected value would only be correct on one platform.
	floor := uint64(imageSize) + oci.RequiredHeadroom(imageSize)

	// Host has 48 GiB free — passes the old 30 GiB floor, fails for a 100 GiB
	// image on every platform (48 GiB is below the floor regardless of margin shape).
	ok, detail := checkDiskHeadroom(48*GiB, imageSize)
	if ok {
		t.Errorf("checkDiskHeadroom(48GiB, 100GiB image) = ok, want fail: host can't pull the image")
	}
	if !strings.Contains(detail, "100") {
		t.Errorf("detail %q should reference image size", detail)
	}

	// Exactly at the floor: must pass. One GiB short: must fail.
	ok, detail = checkDiskHeadroom(floor, imageSize)
	if !ok {
		t.Errorf("checkDiskHeadroom(floor, 100GiB image) = fail %q, want ok", detail)
	}
	ok, _ = checkDiskHeadroom(floor-GiB, imageSize)
	if ok {
		t.Error("checkDiskHeadroom(floor-1GiB, 100GiB image) = ok, want fail")
	}

	// No image size (maxImageBytes = 0): falls back to 30 GiB floor —
	// RequiredHeadroom(0) never exceeds it on any platform (windows' margin
	// is total-relative, and total is 0 here).
	ok, _ = checkDiskHeadroom(29*GiB, 0)
	if ok {
		t.Error("checkDiskHeadroom(29GiB, no image) = ok, want fail: below 30 GiB fallback floor")
	}
	ok, _ = checkDiskHeadroom(31*GiB, 0)
	if !ok {
		t.Error("checkDiskHeadroom(31GiB, no image) = fail, want ok: above 30 GiB fallback floor")
	}

	// Non-GiB-aligned image size: exact byte comparison catches this without
	// ceiling division.
	nonAligned := int64(100<<30) + int64(512<<20) // 100.5 GiB
	nonAlignedFloor := uint64(nonAligned) + oci.RequiredHeadroom(nonAligned)
	ok, _ = checkDiskHeadroom(nonAlignedFloor-GiB, nonAligned)
	if ok {
		t.Error("checkDiskHeadroom(floor-1GiB, 100.5GiB image) = ok, want fail")
	}
	ok, _ = checkDiskHeadroom(nonAlignedFloor+GiB, nonAligned)
	if !ok {
		t.Error("checkDiskHeadroom(floor+1GiB, 100.5GiB image) = fail, want ok")
	}
}

// writeDaemonPlist writes a minimal LaunchDaemon plist pointing program as
// ProgramArguments[0] and returns the path — the same fixture parseDeferralCheck
// uses to resolve the respawn target via TargetPath.
func writeDaemonPlist(t *testing.T, program string) string {
	t.Helper()
	dir := t.TempDir()
	// Write a stub binary so EvalSymlinks succeeds.
	bin := filepath.Join(dir, "runnyd-stub")
	if err := os.WriteFile(bin, []byte("stub"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "runnyd")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	// Use the provided program path if set; otherwise use our local stub.
	target := program
	if target == "" {
		target = link
	}
	data := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>ProgramArguments</key><array><string>` + target + `</string></array>
</dict></plist>`)
	p := filepath.Join(dir, "daemon.plist")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// parseDeferralCheck pure-function coverage: (own-parse-fails × deferred ×
// target verdict) → proceed or named refusal.
func TestParseDeferralCheck(t *testing.T) {
	configParseFail := []socket.DoctorCheck{{Name: "config-parse", OK: false, Detail: "yaml: error at line 1"}}
	alwaysOK := configTester(func(_ context.Context, _, _ string) bool { return true })
	alwaysNo := configTester(func(_ context.Context, _, _ string) bool { return false })

	// A plist that TargetPath can resolve (any existing file as the target).
	plistPath := writeDaemonPlist(t, "")

	cases := []struct {
		name     string
		failed   []socket.DoctorCheck
		deferred bool
		test     configTester
		wantLen  int
		wantName string
	}{
		{
			name:   "not deferred: config-parse passes through",
			failed: configParseFail, deferred: false, test: alwaysOK,
			wantLen: 1, wantName: "config-parse",
		},
		{
			name:   "deferred + target accepts: cleared",
			failed: configParseFail, deferred: true, test: alwaysOK,
			wantLen: 0,
		},
		{
			name:   "deferred + target refuses: respawn-target-config",
			failed: configParseFail, deferred: true, test: alwaysNo,
			wantLen: 1, wantName: "respawn-target-config",
		},
		{
			name: "deferred but multiple failures: passes through unchanged",
			failed: []socket.DoctorCheck{
				{Name: "config-parse", OK: false},
				{Name: "github-client:mac", OK: false},
			},
			deferred: true, test: alwaysOK,
			wantLen: 2,
		},
		{
			name:     "deferred but not config-parse: passes through",
			failed:   []socket.DoctorCheck{{Name: "macos-guest-cap", OK: false}},
			deferred: true, test: alwaysOK,
			wantLen: 1, wantName: "macos-guest-cap",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDeferralCheck(context.Background(), "", plistPath, tc.failed, tc.deferred, tc.test)
			if len(got) != tc.wantLen {
				t.Fatalf("len(got)=%d, want %d; got=%+v", len(got), tc.wantLen, got)
			}
			if tc.wantName != "" && (len(got) == 0 || got[0].Name != tc.wantName) {
				t.Errorf("got[0].Name=%q, want %q", got[0].Name, tc.wantName)
			}
		})
	}
}

// When TargetPath can't resolve (no plist), deferred deferral must refuse with
// respawn-target-config rather than silently accepting — the stale-symlink guard.
func TestParseDeferralCheckNoResolvedTarget(t *testing.T) {
	configParseFail := []socket.DoctorCheck{{Name: "config-parse", OK: false, Detail: "yaml: error"}}
	alwaysOK := configTester(func(_ context.Context, _, _ string) bool { return true })
	got := parseDeferralCheck(context.Background(), "", "/nonexistent/plist.xml", configParseFail, true, alwaysOK)
	if len(got) != 1 || got[0].Name != "respawn-target-config" {
		t.Errorf("unresolvable target: got %+v, want [{Name: respawn-target-config}]", got)
	}
}

// An empty plist path is the non-system-daemon signal (deferralPlistPath returns
// "" off the system home): deferral is system-daemon-only, so the original
// config-parse failure must stand UNCHANGED — not the misleading
// respawn-target-config (which would tell the operator to check a symlink that
// has nothing to do with a per-user agent's bundle-relative respawn).
func TestParseDeferralCheckNonSystemDaemon(t *testing.T) {
	configParseFail := []socket.DoctorCheck{{Name: "config-parse", OK: false, Detail: "yaml: error"}}
	alwaysOK := configTester(func(_ context.Context, _, _ string) bool { return true })
	got := parseDeferralCheck(context.Background(), "", "", configParseFail, true, alwaysOK)
	if len(got) != 1 || got[0].Name != "config-parse" {
		t.Errorf("non-system daemon: got %+v, want the original config-parse failure unchanged", got)
	}
}

// deferralPlistPath yields a path to consult ONLY for the system daemon, and
// only on a platform that has a respawn target at all. A per-user agent (any
// other home) gets "" because it respawns from a bundle-relative BundleProgram,
// not this plist, and consulting the system plist would test the wrong binary
// (mirrors respawn.TargetVersion's home guard). Off darwin every home gets ""
// because no newer binary can be staged at the respawn path in the first place
// (systemRespawnTargetPath).
func TestDeferralPlistPath(t *testing.T) {
	got := deferralPlistPath(home.Dir(home.SystemHomeDir))
	if runtime.GOOS == "darwin" {
		// The system-daemon path must be the SAME plist respawn.TargetPath then
		// reads; asserting equality (not just non-empty) catches a drift between
		// the path the gate hands out and the one launchd actually respawns from.
		if want := sysdaemon.PlistPath(); got != want {
			t.Errorf("system daemon: deferralPlistPath = %q, want %q", got, want)
		}
	} else if got != "" {
		// The regression assertion for the hang: a non-empty path here is what
		// drained a Windows fleet and then held the exit forever, re-consulting a
		// file that could never exist.
		t.Errorf("system daemon on %s: deferralPlistPath = %q, want empty", runtime.GOOS, got)
	}
	if got := deferralPlistPath(home.Dir("/Users/someone/.runny")); got != "" {
		t.Errorf("per-user agent: deferralPlistPath = %q, want empty", got)
	}
}

// exitConfigVerdict: the authority is whichever binary respawns. A normal drain
// runs this binary's local checks; an UpgradeReload drain (deferred) defers to
// the respawn target, re-validating a mid-drain edit (sha != acceptedSHA) but
// trusting already-vetted unchanged bytes without a re-exec.
func TestExitConfigVerdict(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	plist := writeDaemonPlist(t, "") // resolvable respawn target for the deferred cases
	accept := configTester(func(_ context.Context, _, _ string) bool { return true })
	refuse := configTester(func(_ context.Context, _, _ string) bool { return false })
	prefix := home.WorstCasePrefix()

	validPath := writeTestConfigFile(t, validConfigYAML(t, 1, "exit"))
	_, validSHA, err := home.LoadConfigSHA(validPath)
	if err != nil {
		t.Fatalf("valid config did not load: %v", err)
	}
	badKeyPath := writeTestConfigFile(t, []byte(`pools:
  - name: mac
    os: linux
    image: ghcr.io/example/img:latest
    count: 1
    target:
      org: example
    github:
      app_id: 1
      private_key_path: /nonexistent/key.pem
`))
	unparseablePath := writeTestConfigFile(t, []byte("pools: [\n"))

	t.Run("normal drain, valid config → proceed", func(t *testing.T) {
		if ok, d := exitConfigVerdict(ctx, log, validPath, prefix, validSHA, false, "", accept); !ok {
			t.Errorf("want proceed, got hold %q", d)
		}
	})
	t.Run("normal drain, bad key → hold on github-client", func(t *testing.T) {
		ok, d := exitConfigVerdict(ctx, log, badKeyPath, prefix, "", false, "", accept)
		if ok || !strings.Contains(d, "github-client") {
			t.Errorf("want hold naming github-client, got ok=%v detail=%q", ok, d)
		}
	})
	t.Run("normal drain, unparseable → hold", func(t *testing.T) {
		ok, d := exitConfigVerdict(ctx, log, unparseablePath, prefix, "", false, "", accept)
		if ok || !strings.Contains(d, "no longer parses") {
			t.Errorf("want parse hold, got ok=%v detail=%q", ok, d)
		}
	})
	t.Run("deferred, unparseable, target accepts → proceed", func(t *testing.T) {
		if ok, d := exitConfigVerdict(ctx, log, unparseablePath, prefix, "oldsha", true, plist, accept); !ok {
			t.Errorf("want proceed when respawn target accepts, got hold %q", d)
		}
	})
	t.Run("deferred, unparseable, target refuses → hold", func(t *testing.T) {
		ok, d := exitConfigVerdict(ctx, log, unparseablePath, prefix, "oldsha", true, plist, refuse)
		if ok || !strings.Contains(d, "not accepted") {
			t.Errorf("want hold, got ok=%v detail=%q", ok, d)
		}
	})
	t.Run("deferred, parseable config, target accepts → proceed", func(t *testing.T) {
		if ok, d := exitConfigVerdict(ctx, log, validPath, prefix, "stalesha", true, plist, accept); !ok {
			t.Errorf("want proceed when the respawn target accepts, got hold %q", d)
		}
	})
	t.Run("deferred, parseable config, target refuses → hold", func(t *testing.T) {
		ok, d := exitConfigVerdict(ctx, log, validPath, prefix, "stalesha", true, plist, refuse)
		if ok || !strings.Contains(d, "not accepted by the respawn target") {
			t.Errorf("want hold, got ok=%v detail=%q", ok, d)
		}
	})
	t.Run("deferred always consults the target, even when sha == acceptedSHA", func(t *testing.T) {
		// No acceptedSHA short-circuit: a plain Reload joining the drain can advance
		// acceptedSHA to bytes vetted only against the OLD binary, so a SHA match is
		// not proof the target accepts. The target's refusal must still hold the exit.
		ok, d := exitConfigVerdict(ctx, log, validPath, prefix, validSHA, true, plist, refuse)
		if ok || !strings.Contains(d, "not accepted by the respawn target") {
			t.Errorf("want hold (target consulted despite sha match), got ok=%v detail=%q", ok, d)
		}
	})
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
      private_key_path: ` + testRSAKeyPath(t) + `
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

// TestSystemRespawnTargetPathIsDarwinOnly pins the platform statement both the
// parse deferral and the UpgradeReload refusal read. An empty path off darwin
// is not a missing feature: a running executable cannot be replaced in place
// there, so no newer binary can be staged at the respawn path. When this
// returned a darwin plist path unconditionally, an UpgradeReload against a
// Windows system daemon drained the fleet and then held the exit forever,
// because the exit gate kept re-consulting a file that could never exist.
func TestSystemRespawnTargetPathIsDarwinOnly(t *testing.T) {
	got := systemRespawnTargetPath()
	if runtime.GOOS == "darwin" {
		if got == "" {
			t.Fatal("darwin must name a respawn target; the parse deferral has nothing to consult without it")
		}
		if got != sysdaemon.PlistPath() {
			t.Errorf("systemRespawnTargetPath() = %q, want the LaunchDaemon plist %q", got, sysdaemon.PlistPath())
		}
		return
	}
	if got != "" {
		t.Errorf("systemRespawnTargetPath() = %q on %s, want empty — a non-empty path here re-arms the drain-and-hang", got, runtime.GOOS)
	}
}
