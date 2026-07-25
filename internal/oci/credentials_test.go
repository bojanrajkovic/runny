package oci

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeDockerConfig(t *testing.T, contents string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

// captureWarnings swaps in a slog handler recording WARN-level messages, so
// the noisy-vs-quiet split below is asserted rather than assumed.
func captureWarnings(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestCredentialsForStaticAuth(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, auth))

	user, pass, ok := credentialsFor("registry.example.com")
	if !ok || user != "alice" || pass != "hunter2" {
		t.Fatalf("credentialsFor() = %q, %q, %v; want alice, hunter2, true", user, pass, ok)
	}
}

func TestCredentialsForUnknownHost(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, auth))

	if _, _, ok := credentialsFor("other.example.com"); ok {
		t.Fatal("expected no credentials for an unconfigured host")
	}
}

// An ABSENT config file is the overwhelmingly common case (every public pull
// on a host that never logged in anywhere) and must stay silent.
func TestCredentialsForNoConfigFileIsQuiet(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials without a config file")
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no warning for an absent config file, got: %s", logs.String())
	}
}

// A config file that exists but cannot be parsed is a misconfiguration, not an
// absence: staying quiet leaves the operator with a bare 401 and no hint that
// the credential file they wrote is unreadable.
func TestCredentialsForMalformedConfigWarns(t *testing.T) {
	writeDockerConfig(t, `not json`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from a malformed config file")
	}
	if !strings.Contains(logs.String(), "config.json") {
		t.Fatalf("expected a warning naming the unparseable config, got: %s", logs.String())
	}
}

// The headless deployment depends on the home's inheriting ACL granting the
// service account read. If that ACL is wrong the read fails with EACCES and
// the pull silently downgrades to anonymous — the operator sees a file they
// can read perfectly well themselves and no trace of the real cause.
func TestCredentialsForUnreadableConfigWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission bits this test relies on")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"auths":{}}`), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from an unreadable config file")
	}
	if !strings.Contains(logs.String(), "config.json") {
		t.Fatalf("expected a warning naming the unreadable config, got: %s", logs.String())
	}
}

// The shape a keychain-backed `docker login` actually leaves behind: an auths
// entry for the host with no "auth" field, next to a global credsStore. The
// operator's config LOOKS like it holds this credential, so runny must say
// why it doesn't rather than pulling anonymously in silence.
func TestCredentialsForKeychainBackedEntryWarns(t *testing.T) {
	writeDockerConfig(t, `{"auths":{"registry.example.com":{}},"credsStore":"osxkeychain"}`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from a helper-backed entry")
	}
	if !strings.Contains(logs.String(), "credential helper") {
		t.Fatalf("expected a warning about the credential helper, got: %s", logs.String())
	}
}

// An explicit per-host helper is an equally clear statement of intent, even
// with no auths entry alongside it.
func TestCredentialsForPerHostHelperWarns(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"osxkeychain"}}`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from a per-host helper")
	}
	if !strings.Contains(logs.String(), "credential helper") {
		t.Fatalf("expected a warning about the credential helper, got: %s", logs.String())
	}
}

// A bare global credsStore with no entry for this host is set on most macOS
// machines and says nothing about THIS registry — warning there would fire on
// every public image pulled, and a warning that always fires is one nobody
// reads.
func TestCredentialsForGlobalCredsStoreAloneIsQuiet(t *testing.T) {
	writeDockerConfig(t, `{"auths":{"other.example.com":{}},"credsStore":"osxkeychain"}`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials for a host absent from the config")
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no warning for an unrelated global credsStore, got: %s", logs.String())
	}
}

// TestFetchTokenSendsBasicAuth checks fetchToken's wiring directly: when a
// matching DOCKER_CONFIG entry exists for the challenge's registry host, the
// token request must carry it as Basic auth.
func TestFetchTokenSendsBasicAuth(t *testing.T) {
	var gotAuth string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"token":"anon-token"}`))
	}))
	t.Cleanup(tokenSrv.Close)

	ref := Ref{Host: "registry.example.com", Name: "test/image", Tag: "latest"}
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, ref.Host, auth))

	c := NewClient()
	challenge := fmt.Sprintf(`Bearer realm=%q,service="fake"`, tokenSrv.URL)
	if err := c.fetchToken(t.Context(), ref, challenge); err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if gotAuth != "Basic "+auth {
		t.Fatalf("token request Authorization = %q, want %q", gotAuth, "Basic "+auth)
	}
}

// TestFetchTokenNoAuthWithoutConfig checks the flip side: with no matching
// credentials configured, the token request must carry no Authorization
// header at all -- the anonymous-pull path this package has always had.
func TestFetchTokenNoAuthWithoutConfig(t *testing.T) {
	var gotAuth string
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"token":"anon-token"}`))
	}))
	t.Cleanup(tokenSrv.Close)

	t.Setenv("DOCKER_CONFIG", t.TempDir())
	ref := Ref{Host: "registry.example.com", Name: "test/image", Tag: "latest"}

	c := NewClient()
	challenge := fmt.Sprintf(`Bearer realm=%q,service="fake"`, tokenSrv.URL)
	if err := c.fetchToken(t.Context(), ref, challenge); err != nil {
		t.Fatalf("fetchToken: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("token request Authorization = %q, want none", gotAuth)
	}
}
