package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/home"
)

func writeTestConfigFile(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// bigHost outsizes anything the test configs ask for, so only the deliberately
// overcommitted case trips the resource warning.
var bigHost = home.HostResources{LogicalCores: 64, PhysicalRAMGB: 256}

// verdictFor validates body against the conservative worst-case prefix (the
// stateless gate's fallback) — deterministic regardless of the test host's
// hostname or persisted instance-id.
func verdictFor(t *testing.T, body []byte, host home.HostResources) home.Verdict {
	t.Helper()
	return testConfigVerdict(writeTestConfigFile(t, body), home.WorstCasePrefix(), host)
}

func errorsContain(errs []string, sub string) bool {
	for _, e := range errs {
		if strings.Contains(e, sub) {
			return true
		}
	}
	return false
}

func TestTestConfigVerdictOK(t *testing.T) {
	// A valid config with a real, parseable private key returns ok — the gate runs
	// the local startup-blocking checks (key parse, guest-cap, namespace,
	// image-ref) but no network, so a well-formed config with a readable key is
	// clean.
	v := verdictFor(t, validConfigYAML(t, 1, "ok"), bigHost)
	if v.Status != "ok" {
		t.Fatalf("status = %q, want ok; errors=%v warnings=%v", v.Status, v.Errors, v.Warnings)
	}
	if len(v.Errors) != 0 || len(v.Warnings) != 0 {
		t.Errorf("ok verdict should be empty: errors=%v warnings=%v", v.Errors, v.Warnings)
	}
}

func TestTestConfigVerdictErrorMissingKey(t *testing.T) {
	// The private key is a local, startup-blocking input: startup's buildClients
	// reads + PEM/RSA-parses it and the respawn crash-loops if it's missing or
	// malformed, so the gate must catch it (same reasoning as the image-ref parse).
	// No network is involved — this is a local file read.
	body := []byte(`pools:
  - name: mac
    os: linux
    image: ghcr.io/example/img:latest
    count: 1
    target:
      org: example
    github:
      app_id: 1
      private_key_path: /nonexistent/key.pem
`)
	v := verdictFor(t, body, bigHost)
	if v.Status != "error" {
		t.Fatalf("status = %q, want error; %+v", v.Status, v)
	}
	if !errorsContain(v.Errors, "github-client") {
		t.Errorf("errors should name github-client (the key parse): %v", v.Errors)
	}
}

func TestTestConfigVerdictWarnDeadline(t *testing.T) {
	body := append(validConfigYAML(t, 1, "warn"), []byte("deadlines:\n  await_ssh: 1s\n")...)
	v := verdictFor(t, body, bigHost)
	if v.Status != "warn" {
		t.Fatalf("status = %q, want warn; %+v", v.Status, v)
	}
	if len(v.Errors) != 0 {
		t.Errorf("warn must carry no errors: %v", v.Errors)
	}
	if len(v.Warnings) != 1 || v.Warnings[0].Kind != home.WarnDeadlineTooShort {
		t.Errorf("want one deadline-too-short warning, got %+v", v.Warnings)
	}
}

func TestTestConfigVerdictWarnOvercommit(t *testing.T) {
	// 2 darwin slots × 40 cores = 80 > 64 logical → resource-overcommit; 2 darwin
	// slots are at (not over) the guest cap, so this is a warn, not an error.
	body := []byte(`pools:
  - name: mac
    os: darwin
    image: ghcr.io/example/img:latest
    count: 2
    cpu_cores: 40
    target:
      org: example
    github:
      app_id: 1
      private_key_path: ` + testRSAKeyPath(t) + `
`)
	v := verdictFor(t, body, bigHost)
	if v.Status != "warn" {
		t.Fatalf("status = %q, want warn; %+v", v.Status, v)
	}
	if len(v.Warnings) != 1 || v.Warnings[0].Kind != home.WarnResourceOvercommit {
		t.Errorf("want one resource-overcommit warning, got %+v", v.Warnings)
	}
}

func TestTestConfigVerdictErrorUnparseable(t *testing.T) {
	v := verdictFor(t, []byte("pools: [oops\n"), bigHost)
	if v.Status != "error" || len(v.Errors) == 0 {
		t.Fatalf("want error with messages, got %+v", v)
	}
}

func TestTestConfigVerdictErrorOverGuestCap(t *testing.T) {
	// 3 darwin slots exceed the 2-guest cap → error.
	v := verdictFor(t, validConfigYAML(t, 3, "overcap"), bigHost)
	if v.Status != "error" {
		t.Fatalf("status = %q, want error; %+v", v.Status, v)
	}
	if !errorsContain(v.Errors, "macos-guest-cap") {
		t.Errorf("errors should name macos-guest-cap: %v", v.Errors)
	}
}

func TestTestConfigVerdictErrorRunnerNamespace(t *testing.T) {
	// A 60-char pool name overflows GitHub's 64-char runner-name cap regardless of
	// the prefix. Linux avoids the guest-cap interfering.
	v := verdictFor(t, longPoolConfig(t, strings.Repeat("a", 60)), bigHost)
	if v.Status != "error" {
		t.Fatalf("status = %q, want error; %+v", v.Status, v)
	}
	if !errorsContain(v.Errors, "runner-namespace") {
		t.Errorf("errors should name runner-namespace: %v", v.Errors)
	}
}

func TestTestConfigVerdictErrorBadImageRef(t *testing.T) {
	// An image that passes validate()'s non-empty check but fails oci.ParseRef
	// (no registry host) → error: startup runs the same parse and refuses to boot.
	body := []byte(`pools:
  - name: mac
    os: linux
    image: not-a-registry-ref
    count: 1
    target:
      org: example
    github:
      app_id: 1
      private_key_path: ` + testRSAKeyPath(t) + `
`)
	v := verdictFor(t, body, bigHost)
	if v.Status != "error" {
		t.Fatalf("status = %q, want error; %+v", v.Status, v)
	}
	if !errorsContain(v.Errors, "image-ref") {
		t.Errorf("errors should name the image-ref problem: %v", v.Errors)
	}
}

func TestTestConfigVerdictUsesConservativePrefix(t *testing.T) {
	// The gate must never UNDER-estimate the prefix into a false OK: a host renamed
	// since first run keeps its longer persisted prefix. A pool name that fits under
	// a short prefix but overflows under the worst-case prefix must be REFUSED.
	body := longPoolConfig(t, strings.Repeat("p", 30))
	path := writeTestConfigFile(t, body)
	// Short prefix: 12 + 1 + 30 + 1 + 1 + 1 + 8 = 54 ≤ 64 → ok.
	if v := testConfigVerdict(path, "mac-00000000", bigHost); v.Status != "ok" {
		t.Fatalf("short prefix: want ok, got %+v", v)
	}
	// Worst-case prefix: 33 + 1 + 30 + ... = 75 > 64 → error (conservative).
	if v := testConfigVerdict(path, home.WorstCasePrefix(), bigHost); v.Status != "error" {
		t.Fatalf("worst-case prefix: want error (gate must not under-estimate), got %+v", v)
	}
}

func TestTestConfigVerdictJSONIsStableContract(t *testing.T) {
	body := append(validConfigYAML(t, 1, "warn"), []byte("deadlines:\n  await_ssh: 1s\n")...)
	b, err := json.Marshal(verdictFor(t, body, bigHost))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"status":"warn"`, `"errors":[`, `"warnings":[`, `"kind":"deadline-too-short"`, `"message":`} {
		if !strings.Contains(s, want) {
			t.Errorf("verdict JSON missing %q: %s", want, s)
		}
	}

	// An ok verdict must marshal empty arrays, not null — the cross-language
	// decoders (Swift app, runnyctl) expect arrays.
	okJSON, err := json.Marshal(verdictFor(t, validConfigYAML(t, 1, "ok"), bigHost))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(okJSON), "null") {
		t.Errorf("ok verdict must not contain null: %s", okJSON)
	}
}

// longPoolConfig is a single-linux-pool config with the given pool name, for
// exercising the runner-name length cap. It carries a real private key so the
// namespace failure is isolated from the github-client (key-parse) check.
func longPoolConfig(t *testing.T, name string) []byte {
	t.Helper()
	return []byte(`pools:
  - name: ` + name + `
    os: linux
    image: ghcr.io/example/img:latest
    count: 1
    target:
      org: example
    github:
      app_id: 1
      private_key_path: ` + testRSAKeyPath(t) + `
`)
}
