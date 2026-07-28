package socket

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bojanrajkovic/runny/internal/home"
	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

// seededConfig is deliberately not canonical YAML: a modeline comment, an
// inline comment, and keys out of alphabetical order. Anything that reparses
// and re-marshals loses at least one of the three, so a byte-for-byte
// comparison against this is what proves the RPC carries the FILE and not a
// document.
const seededConfig = `# yaml-language-server: $schema=https://example.invalid/config.schema.json
pools:
  - name: zeta   # trailing comment
    os: linux
`

func configServer(t *testing.T) *Server {
	t.Helper()
	return &Server{HomeDir: home.Dir(t.TempDir())}
}

func TestGetConfigReturnsTheFileVerbatim(t *testing.T) {
	s := configServer(t)
	if err := os.WriteFile(s.HomeDir.ConfigPath(), []byte(seededConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	resp, err := s.GetConfig(t.Context(), &runnyv1.GetConfigRequest{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if got := string(resp.Content); got != seededConfig {
		t.Errorf("content round-tripped lossily:\n got %q\nwant %q", got, seededConfig)
	}
	if resp.Path != s.HomeDir.ConfigPath() {
		t.Errorf("path = %q, want %q", resp.Path, s.HomeDir.ConfigPath())
	}
}

// NotFound must be its own code. edit-config seeds a blank skeleton on "no
// config exists", and that is destructive if it also fires for a config that
// merely could not be read — so the two answers can never share a code.
func TestGetConfigReportsNotFoundForAMissingConfig(t *testing.T) {
	s := configServer(t)
	_, err := s.GetConfig(t.Context(), &runnyv1.GetConfigRequest{})
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("code = %s (err %v), want NotFound", got, err)
	}
}

// An unreadable config is NOT NotFound: it is exactly the case the skeleton
// fallback must refuse to handle. Skipped as root, which can read anything.
func TestGetConfigDoesNotReportAnUnreadableConfigAsMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through mode bits; the distinction is unobservable")
	}
	s := configServer(t)
	if err := os.WriteFile(s.HomeDir.ConfigPath(), []byte(seededConfig), 0o000); err != nil {
		t.Fatal(err)
	}

	_, err := s.GetConfig(t.Context(), &runnyv1.GetConfigRequest{})
	if got := status.Code(err); got == codes.NotFound {
		t.Fatalf("an unreadable config reported as NotFound; the client would seed a blank skeleton over it")
	} else if got == codes.OK {
		t.Fatalf("expected an error reading a mode-000 config")
	}
}

func TestSetConfigWritesTheBytesAndReturnsTheirDigest(t *testing.T) {
	s := configServer(t)

	resp, err := s.SetConfig(t.Context(), &runnyv1.SetConfigRequest{Content: []byte(seededConfig)})
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	on, err := os.ReadFile(s.HomeDir.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(on) != seededConfig {
		t.Errorf("on-disk config:\n got %q\nwant %q", on, seededConfig)
	}
	if want := fmt.Sprintf("%x", sha256.Sum256([]byte(seededConfig))); resp.ConfigSha256 != want {
		t.Errorf("config_sha256 = %q, want %q", resp.ConfigSha256, want)
	}
	if resp.Path != s.HomeDir.ConfigPath() {
		t.Errorf("path = %q, want %q", resp.Path, s.HomeDir.ConfigPath())
	}
}

// The write replaces an existing file and leaves nothing behind. A stray temp
// file in the home is not cosmetic: the daemon's own home is where `prune` and
// the doctor look, and litter accumulates one file per edit.
func TestSetConfigReplacesAndLeavesNoTempBehind(t *testing.T) {
	s := configServer(t)
	if err := os.WriteFile(s.HomeDir.ConfigPath(), []byte("pools: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.SetConfig(t.Context(), &runnyv1.SetConfigRequest{Content: []byte(seededConfig)}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	on, err := os.ReadFile(s.HomeDir.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(on) != seededConfig {
		t.Errorf("config was not replaced:\n got %q\nwant %q", on, seededConfig)
	}
	entries, err := os.ReadDir(s.HomeDir.String())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(s.HomeDir.ConfigPath()) {
			t.Errorf("left %s behind in the home", e.Name())
		}
	}
}

// Empty content is a real edit (a config emptied to nothing), not a malformed
// request — the daemon persists what it is given and Reload's exit gate is
// what refuses to respawn onto it.
func TestSetConfigAcceptsEmptyContent(t *testing.T) {
	s := configServer(t)
	if _, err := s.SetConfig(t.Context(), &runnyv1.SetConfigRequest{}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	on, err := os.ReadFile(s.HomeDir.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != 0 {
		t.Errorf("on-disk config = %q, want empty", on)
	}
}

// A home that does not exist is a broken daemon, not a client error: say so
// with the path, rather than a bare ENOENT that reads like a missing config.
func TestSetConfigFailsLoudlyWithoutAHome(t *testing.T) {
	s := &Server{HomeDir: home.Dir(filepath.Join(t.TempDir(), "absent"))}
	_, err := s.SetConfig(t.Context(), &runnyv1.SetConfigRequest{Content: []byte(seededConfig)})
	if err == nil {
		t.Fatal("expected an error writing into a home that does not exist")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error %q does not name the path it failed on", err)
	}
}
