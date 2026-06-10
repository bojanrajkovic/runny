package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeGitHub stubs the five endpoints the client touches, verifying the App
// JWT signature against the test keypair on the way.
type fakeGitHub struct {
	t             *testing.T
	pub           *rsa.PublicKey
	adminPerm     string
	orgRunnerPerm string
	tokenMints    atomic.Int32
	jitCalls      atomic.Int32
	failJITN      int32 // first N generate-jitconfig calls answer 503
	runners       []Runner
	deleted       []int64
}

func (f *fakeGitHub) handler() http.Handler {
	mux := http.NewServeMux()
	installation := func(w http.ResponseWriter, r *http.Request) {
		f.verifyJWT(r)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 77})
	}
	mux.HandleFunc("GET /repos/o/r/installation", installation)
	mux.HandleFunc("GET /orgs/myorg/installation", installation)
	mux.HandleFunc("POST /app/installations/77/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.verifyJWT(r)
		f.tokenMints.Add(1)
		perms := map[string]string{"administration": f.adminPerm, "contents": "read"}
		if f.orgRunnerPerm != "" {
			perms["organization_self_hosted_runners"] = f.orgRunnerPerm
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":       "ghs_testtoken",
			"expires_at":  time.Now().Add(time.Hour).Format(time.RFC3339),
			"permissions": perms,
		})
	})
	jitconfig := func(w http.ResponseWriter, r *http.Request) {
		f.requireToken(r)
		if f.jitCalls.Add(1) <= f.failJITN {
			http.Error(w, "upstream wobble", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Name          string   `json:"name"`
			RunnerGroupID int64    `json:"runner_group_id"`
			Labels        []string `json:"labels"`
			WorkFolder    string   `json:"work_folder"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.RunnerGroupID != 1 || body.WorkFolder != "_work" {
			f.t.Errorf("jitconfig body: %+v", body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"runner":             map[string]any{"id": 4242, "name": body.Name},
			"encoded_jit_config": "ZmFrZS1qaXQ=",
		})
	}
	mux.HandleFunc("POST /repos/o/r/actions/runners/generate-jitconfig", jitconfig)
	mux.HandleFunc("POST /orgs/myorg/actions/runners/generate-jitconfig", jitconfig)
	downloads := func(w http.ResponseWriter, r *http.Request) {
		f.requireToken(r)
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"os": "osx", "architecture": "arm64", "filename": "actions-runner-osx-arm64-2.334.0.tar.gz", "download_url": "https://example/osx"},
			{"os": "linux", "architecture": "arm64", "filename": "actions-runner-linux-arm64-2.334.0.tar.gz", "download_url": "https://example/linux"},
			{"os": "linux", "architecture": "x64", "filename": "actions-runner-linux-x64-2.334.0.tar.gz", "download_url": "https://example/x64"},
		})
	}
	mux.HandleFunc("GET /repos/o/r/actions/runners/downloads", downloads)
	mux.HandleFunc("GET /orgs/myorg/actions/runners/downloads", downloads)
	mux.HandleFunc("GET /repos/o/r/actions/runners", func(w http.ResponseWriter, r *http.Request) {
		f.requireToken(r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": len(f.runners), "runners": f.runners,
		})
	})
	mux.HandleFunc("DELETE /repos/o/r/actions/runners/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.requireToken(r)
		var id int64
		_, _ = fmt.Sscan(r.PathValue("id"), &id)
		for _, known := range f.runners {
			if known.ID == id {
				f.deleted = append(f.deleted, id)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, "not found", http.StatusNotFound)
	})
	return mux
}

func (f *fakeGitHub) verifyJWT(r *http.Request) {
	raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) { return f.pub, nil },
		jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !tok.Valid {
		f.t.Errorf("app JWT invalid: %v", err)
	}
	if iss, _ := tok.Claims.GetIssuer(); iss != "2798371" {
		f.t.Errorf("jwt iss = %q", iss)
	}
}

func (f *fakeGitHub) requireToken(r *http.Request) {
	if r.Header.Get("Authorization") != "token ghs_testtoken" {
		f.t.Errorf("missing installation token, got %q", r.Header.Get("Authorization"))
	}
}

func newTestClient(t *testing.T, f *fakeGitHub) *Client {
	return newTestClientTarget(t, f, Target{Owner: "o", Repo: "r"})
}

func newTestClientTarget(t *testing.T, f *fakeGitHub, target Target) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f.t, f.pub = t, &key.PublicKey
	if f.adminPerm == "" {
		f.adminPerm = "write"
	}
	keyPath := filepath.Join(t.TempDir(), "key.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 2798371, PrivateKeyPath: keyPath, APIBase: srv.URL}, target)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestGenerateJITConfig(t *testing.T) {
	f := &fakeGitHub{}
	c := newTestClient(t, f)
	jit, err := c.GenerateJITConfig(t.Context(), "runny-1-abcd1234", []string{"self-hosted", "macOS"}, 1)
	if err != nil {
		t.Fatalf("GenerateJITConfig: %v", err)
	}
	if jit.RunnerID != 4242 || jit.EncodedJITConfig != "ZmFrZS1qaXQ=" {
		t.Errorf("jit = %+v", jit)
	}
}

func TestTokenCaching(t *testing.T) {
	f := &fakeGitHub{}
	c := newTestClient(t, f)
	for range 3 {
		if _, err := c.GenerateJITConfig(t.Context(), "n", nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	if mints := f.tokenMints.Load(); mints != 1 {
		t.Errorf("token minted %d times across 3 calls, want 1 (caching broken)", mints)
	}
}

func TestJITRetryOn5xx(t *testing.T) {
	f := &fakeGitHub{failJITN: 2}
	c := newTestClient(t, f)
	jit, err := c.GenerateJITConfig(t.Context(), "n", nil, 1)
	if err != nil || jit.RunnerID != 4242 {
		t.Fatalf("retry should have recovered: %v", err)
	}
	if calls := f.jitCalls.Load(); calls != 3 {
		t.Errorf("jit called %d times, want 3", calls)
	}
}

func TestCheckRunnerPerm(t *testing.T) {
	c := newTestClient(t, &fakeGitHub{adminPerm: "write"})
	if err := c.CheckRunnerPerm(t.Context()); err != nil {
		t.Errorf("repo target with administration:write: %v", err)
	}
	c2 := newTestClient(t, &fakeGitHub{adminPerm: "read"})
	if err := c2.CheckRunnerPerm(t.Context()); !errors.Is(err, ErrMissingRunnerPerm) {
		t.Errorf("want ErrMissingRunnerPerm, got %v", err)
	}
	// Org target needs the ORG permission; administration alone is not enough.
	org := Target{Org: "myorg"}
	c3 := newTestClientTarget(t, &fakeGitHub{adminPerm: "write", orgRunnerPerm: "write"}, org)
	if err := c3.CheckRunnerPerm(t.Context()); err != nil {
		t.Errorf("org target with org runner perm: %v", err)
	}
	c4 := newTestClientTarget(t, &fakeGitHub{adminPerm: "write"}, org)
	if err := c4.CheckRunnerPerm(t.Context()); !errors.Is(err, ErrMissingRunnerPerm) {
		t.Errorf("org target without org perm: want ErrMissingRunnerPerm, got %v", err)
	}
}

func TestOrgTargetJITConfig(t *testing.T) {
	c := newTestClientTarget(t, &fakeGitHub{}, Target{Org: "myorg"})
	jit, err := c.GenerateJITConfig(t.Context(), "runny-lin-1-abcd1234", []string{"self-hosted"}, 1)
	if err != nil || jit.RunnerID != 4242 {
		t.Fatalf("org jitconfig: %+v, %v", jit, err)
	}
}

func TestRunnerDownloadPerOS(t *testing.T) {
	c := newTestClient(t, &fakeGitHub{})
	name, url, err := c.RunnerDownload(t.Context(), "darwin")
	if err != nil || !strings.Contains(name, "osx-arm64") || url != "https://example/osx" {
		t.Errorf("darwin: %s %s %v", name, url, err)
	}
	name, url, err = c.RunnerDownload(t.Context(), "linux")
	if err != nil || !strings.Contains(name, "linux-arm64") || url != "https://example/linux" {
		t.Errorf("linux: %s %s %v", name, url, err)
	}
	if _, _, err := c.RunnerDownload(t.Context(), "windows"); err == nil {
		t.Error("windows should be rejected")
	}
}

func TestListAndDeleteRunners(t *testing.T) {
	f := &fakeGitHub{runners: []Runner{
		{ID: 1, Name: "runny-1-aaaa", Status: "online"},
		{ID: 2, Name: "runny-2-bbbb", Status: "offline"},
	}}
	c := newTestClient(t, f)
	rs, err := c.ListRunners(t.Context())
	if err != nil || len(rs) != 2 {
		t.Fatalf("ListRunners: %v, %v", rs, err)
	}
	if err := c.DeleteRunner(t.Context(), 2); err != nil {
		t.Errorf("DeleteRunner: %v", err)
	}
	// 404 is success — the runner already removed itself after its job.
	if err := c.DeleteRunner(t.Context(), 999); err != nil {
		t.Errorf("DeleteRunner on missing id should be nil, got %v", err)
	}
}
