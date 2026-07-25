package oci

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestCredentialsForStaticAuth(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, auth))

	user, pass, ok := credentialsFor(t.Context(), "registry.example.com")
	if !ok || user != "alice" || pass != "hunter2" {
		t.Fatalf("credentialsFor() = %q, %q, %v; want alice, hunter2, true", user, pass, ok)
	}
}

func TestCredentialsForNoConfigFile(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // dir exists, config.json does not
	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials without a config file")
	}
}

func TestCredentialsForUnknownHost(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, auth))

	if _, _, ok := credentialsFor(t.Context(), "other.example.com"); ok {
		t.Fatal("expected no credentials for an unconfigured host")
	}
}

func TestCredentialsForMalformedConfig(t *testing.T) {
	writeDockerConfig(t, `not json`)
	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials from a malformed config file")
	}
}

// stubCredentialHelper writes a docker-credential-<name> script onto PATH
// that echoes fixed JSON, standing in for the real helper protocol (host on
// stdin, {"Username","Secret"} JSON on stdout) without touching a real
// credential store.
func stubCredentialHelper(t *testing.T, name, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub helper script is a POSIX shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "docker-credential-"+name)
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCredentialsForHelper(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"stub"}}`)
	stubCredentialHelper(t, "stub", `echo '{"Username":"bob","Secret":"s3cr3t"}'`)

	user, pass, ok := credentialsFor(t.Context(), "registry.example.com")
	if !ok || user != "bob" || pass != "s3cr3t" {
		t.Fatalf("credentialsFor() = %q, %q, %v; want bob, s3cr3t, true", user, pass, ok)
	}
}

func TestCredentialsForCredsStoreFallback(t *testing.T) {
	writeDockerConfig(t, `{"credsStore":"stub"}`)
	stubCredentialHelper(t, "stub", `echo '{"Username":"bob","Secret":"s3cr3t"}'`)

	user, pass, ok := credentialsFor(t.Context(), "registry.example.com")
	if !ok || user != "bob" || pass != "s3cr3t" {
		t.Fatalf("credentialsFor() = %q, %q, %v; want bob, s3cr3t, true", user, pass, ok)
	}
}

func TestCredentialsForHelperMissingBinary(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"does-not-exist"}}`)
	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials when the helper binary is missing")
	}
}

func TestCredentialsForHelperNonZeroExit(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"stub"}}`)
	stubCredentialHelper(t, "stub", `exit 1`)

	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials when the helper exits non-zero")
	}
}

// A keychain-backed `docker login` leaves an auths ENTRY for the host with no
// "auth" field -- the secret lives in the store, and the map is just an index
// of which hosts have one. That stub must not short-circuit the lookup as
// "found"; it has to fall through to the configured helper.
func TestCredentialsForStubAuthEntryFallsThroughToHelper(t *testing.T) {
	writeDockerConfig(t, `{"auths":{"registry.example.com":{}},"credsStore":"stub"}`)
	stubCredentialHelper(t, "stub", `echo '{"Username":"bob","Secret":"s3cr3t"}'`)

	user, pass, ok := credentialsFor(t.Context(), "registry.example.com")
	if !ok || user != "bob" || pass != "s3cr3t" {
		t.Fatalf("credentialsFor() = %q, %q, %v; want bob, s3cr3t, true", user, pass, ok)
	}
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

// A helper answering "credentials not found" is the normal reply for any host
// it holds nothing for -- which is every public image pulled on a machine with
// a global credsStore. That path must stay silent or the warning fires on
// nearly every pull.
func TestCredentialsForHelperNotFoundIsQuiet(t *testing.T) {
	writeDockerConfig(t, `{"credsStore":"stub"}`)
	stubCredentialHelper(t, "stub", `echo "credentials not found in native keychain" >&2; exit 1`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials when the helper has none for the host")
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no warning for a not-found helper reply, got: %s", logs.String())
	}
}

// A helper that is missing or broken IS worth a warning: the operator
// configured it, and a silent downgrade to anonymous surfaces later as a bare
// 401 with no trace of the real cause.
func TestCredentialsForBrokenHelperWarns(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"does-not-exist"}}`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials when the helper binary is missing")
	}
	if !strings.Contains(logs.String(), "docker-credential-does-not-exist") {
		t.Fatalf("expected a warning naming the missing helper, got: %s", logs.String())
	}
}

func TestCredentialsForHelperEmptySecret(t *testing.T) {
	writeDockerConfig(t, `{"credHelpers":{"registry.example.com":"stub"}}`)
	stubCredentialHelper(t, "stub", `echo '{"Username":"bob","Secret":""}'`)

	if _, _, ok := credentialsFor(t.Context(), "registry.example.com"); ok {
		t.Fatal("expected no credentials when the helper returns an empty secret")
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
