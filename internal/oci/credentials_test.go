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

// A config that is PRESENT but unreadable must warn, unlike an absent one.
// In production this is the home's inherited service entry failing to reach the
// account read: the pull silently downgrades to anonymous while the operator
// sees a file they can read perfectly well themselves. Reproduced here with a
// directory standing in for the file, which fails the read on every platform —
// chmod 0o000 does not, since Windows maps mode bits only to the read-only
// attribute and would leave the file readable.
func TestCredentialsForUnreadableConfigWarns(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "config.json"), 0o755); err != nil {
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

// docs/deploy.md has the operator base64 the credential by hand, so a
// truncated or mangled string is a likely mistake. It must not look identical
// to having configured nothing -- that is a bare 401 with no trace of why.
func TestCredentialsForUndecodableAuthWarns(t *testing.T) {
	writeDockerConfig(t, `{"auths":{"registry.example.com":{"auth":"!!!not-base64!!!"}}}`)
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from an undecodable auth entry")
	}
	if !strings.Contains(logs.String(), "decodable") {
		t.Fatalf("expected a warning about the undecodable entry, got: %s", logs.String())
	}
}

// Valid base64 that does not contain a colon is the same class of typo.
func TestCredentialsForAuthWithoutColonWarns(t *testing.T) {
	noColon := base64.StdEncoding.EncodeToString([]byte("alice-no-colon"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{"registry.example.com":{"auth":%q}}}`, noColon))
	logs := captureWarnings(t)

	if _, _, ok := credentialsFor("registry.example.com"); ok {
		t.Fatal("expected no credentials from an auth entry with no colon")
	}
	if !strings.Contains(logs.String(), "decodable") {
		t.Fatalf("expected a warning about the malformed entry, got: %s", logs.String())
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

// basicOnlyRegistry answers every request with a Basic challenge until the
// right credentials arrive — a bare Distribution behind htpasswd, which has no
// token endpoint at all.
func basicOnlyRegistry(t *testing.T, wantUser, wantPass string) (*httptest.Server, Ref) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != wantUser || pass != wantPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Registry Realm"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"layers":[]}`))
	}))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return srv, Ref{Host: host, Name: "test/image", Tag: "latest"}
}

// A Basic challenge must send the credentials straight back to the registry.
// Routing it through the token dance fails: its realm is a human label, not a
// token endpoint.
func TestGetRetriesBasicChallengeAgainstRegistry(t *testing.T) {
	_, ref := basicOnlyRegistry(t, "alice", "hunter2")
	auth := base64.StdEncoding.EncodeToString([]byte("alice:hunter2"))
	writeDockerConfig(t, fmt.Sprintf(`{"auths":{%q:{"auth":%q}}}`, ref.Host, auth))

	c := NewClient()
	if _, _, _, err := c.fetchManifest(t.Context(), ref); err != nil {
		t.Fatalf("fetchManifest against a Basic-only registry: %v", err)
	}
}

// With no credentials configured, a Basic challenge must fail with a message
// naming the cause -- not the token flow's "missing realm", which describes
// nothing an operator can act on.
func TestGetBasicChallengeWithoutCredentialsIsLegible(t *testing.T) {
	_, ref := basicOnlyRegistry(t, "alice", "hunter2")
	t.Setenv("DOCKER_CONFIG", t.TempDir())

	c := NewClient()
	_, _, _, err := c.fetchManifest(t.Context(), ref)
	if err == nil {
		t.Fatal("expected an error with no credentials configured")
	}
	if !strings.Contains(err.Error(), "Basic authentication") {
		t.Fatalf("error should name Basic auth as the cause, got: %v", err)
	}
}

// Go copies Authorization across a redirect on a hostname match without
// comparing schemes, so an https registry answering 302 to http on the same
// host would re-send the credential in the clear -- a static registry
// password, not a scoped token. The client must refuse the hop.
func TestClientRefusesPlaintextRedirect(t *testing.T) {
	var leaked string
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
	}))
	t.Cleanup(plain.Close)

	// A non-loopback Host header is what makes this a downgrade rather than
	// the permitted local-registry case; the request still dials the test
	// server, but the URL the redirect policy sees is a public-looking http one.
	target := "http://registry.example.com" + "/v2/"
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)

	c := NewClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirector.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth("alice", "hunter2")
	resp, err := c.hc.Do(req)
	if err == nil {
		resp.Body.Close()
		t.Fatal("expected the plaintext redirect to be refused")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("error should name the plaintext downgrade, got: %v", err)
	}
	if leaked != "" {
		t.Fatalf("credentials leaked over cleartext: %q", leaked)
	}
}

// A cross-host HTTPS redirect is legitimate -- registries hand blobs off to a
// CDN that way -- and must NOT be refused by the downgrade guard.
func TestClientAllowsCrossHostHTTPSRedirect(t *testing.T) {
	c := NewClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://registry.example.com/v2/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.hc.CheckRedirect(req, nil); err != nil {
		t.Fatalf("https redirect should be allowed, got: %v", err)
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
