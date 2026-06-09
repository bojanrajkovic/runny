// Package github implements the App-auth → JIT-config chain of ADR-0003 and
// the runner reconcile/delete surface the FSM's health logic needs. It speaks
// the REST API directly — the dependency footprint of a full client library
// isn't warranted for five endpoints.
package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ErrNoAdminWrite is returned when the App's installation token does not
// carry administration:write — configured permissions can lag granted ones
// (per-installation approval), so doctor asserts on a *minted* token.
var ErrNoAdminWrite = errors.New("installation token lacks administration:write")

// Config mirrors home.GitHubConfig without importing it (no dependency cycle;
// the daemon maps one to the other).
type Config struct {
	AppID          int64
	PrivateKeyPath string
	Owner          string
	Repo           string
	APIBase        string
}

// Client is a minimal GitHub App client scoped to one repo.
type Client struct {
	cfg Config
	key *rsa.PrivateKey
	hc  *http.Client

	mu         sync.Mutex
	token      string
	tokenPerms map[string]string
	tokenExp   time.Time
	instID     int64
}

func New(cfg Config) (*Client, error) {
	raw, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading app private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("app private key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		var k any
		k, err = x509.ParsePKCS8PrivateKey(block.Bytes)
		if err == nil {
			var ok bool
			if key, ok = k.(*rsa.PrivateKey); !ok {
				err = errors.New("not an RSA key")
			}
		}
	}
	if err != nil {
		return nil, fmt.Errorf("parsing app private key: %w", err)
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.github.com"
	}
	return &Client{cfg: cfg, key: key, hc: &http.Client{Timeout: 30 * time.Second}}, nil
}

// appJWT mints the short-lived RS256 App JWT (iss=app_id, ≤10min).
func (c *Client) appJWT() (string, error) {
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iat": now.Add(-30 * time.Second).Unix(), // clock-skew guard
		"exp": now.Add(9 * time.Minute).Unix(),
		// iss as string per RFC 7519 registered-claim typing; GitHub accepts it.
		"iss": strconv.FormatInt(c.cfg.AppID, 10),
	})
	return tok.SignedString(c.key)
}

// installationToken returns a cached installation token, minting a fresh one
// when within 5 minutes of expiry.
func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.tokenExp) > 5*time.Minute {
		return c.token, nil
	}

	jwtStr, err := c.appJWT()
	if err != nil {
		return "", fmt.Errorf("minting app jwt: %w", err)
	}
	if c.instID == 0 {
		var inst struct {
			ID int64 `json:"id"`
		}
		err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/%s/installation", c.cfg.Owner, c.cfg.Repo),
			"Bearer "+jwtStr, nil, &inst)
		if err != nil {
			return "", fmt.Errorf("resolving installation: %w", err)
		}
		c.instID = inst.ID
	}

	var tok struct {
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expires_at"`
		Permissions map[string]string `json:"permissions"`
	}
	err = c.do(ctx, http.MethodPost,
		fmt.Sprintf("/app/installations/%d/access_tokens", c.instID),
		"Bearer "+jwtStr, nil, &tok)
	if err != nil {
		return "", fmt.Errorf("minting installation token: %w", err)
	}
	c.token, c.tokenExp, c.tokenPerms = tok.Token, tok.ExpiresAt, tok.Permissions
	return c.token, nil
}

// CheckAdminWrite mints (or reuses) a token and asserts administration:write
// is actually present — the doctor check ADR-0003 calls for.
func (c *Client) CheckAdminWrite(ctx context.Context) error {
	if _, err := c.installationToken(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokenPerms["administration"] != "write" {
		return fmt.Errorf("%w (got %q)", ErrNoAdminWrite, c.tokenPerms["administration"])
	}
	return nil
}

// JITRunner is the result of generate-jitconfig: a registered (offline)
// runner and the encoded blob the guest's run.sh consumes.
type JITRunner struct {
	RunnerID         int64
	EncodedJITConfig string
}

// GenerateJITConfig registers an ephemeral runner and returns its one-shot
// config. The runner auto-removes after one job; if it never takes one, the
// caller must DeleteRunner on teardown (ADR-0003).
func (c *Client) GenerateJITConfig(ctx context.Context, name string, labels []string, groupID int64) (*JITRunner, error) {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"name":            name,
		"runner_group_id": groupID,
		"labels":          labels,
		"work_folder":     "_work",
	}
	var resp struct {
		Runner struct {
			ID int64 `json:"id"`
		} `json:"runner"`
		EncodedJITConfig string `json:"encoded_jit_config"`
	}
	err = c.doRetry(ctx, http.MethodPost,
		fmt.Sprintf("/repos/%s/%s/actions/runners/generate-jitconfig", c.cfg.Owner, c.cfg.Repo),
		"token "+tok, body, &resp)
	if err != nil {
		return nil, fmt.Errorf("generate-jitconfig: %w", err)
	}
	return &JITRunner{RunnerID: resp.Runner.ID, EncodedJITConfig: resp.EncodedJITConfig}, nil
}

// Runner is a registered self-hosted runner, as the reconcile loop sees it.
type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // "online" | "offline"
	Busy   bool   `json:"busy"`
}

// ListRunners returns all self-hosted runners on the repo (paginated).
func (c *Client) ListRunners(ctx context.Context) ([]Runner, error) {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return nil, err
	}
	var all []Runner
	for page := 1; ; page++ {
		var resp struct {
			TotalCount int      `json:"total_count"`
			Runners    []Runner `json:"runners"`
		}
		err := c.doRetry(ctx, http.MethodGet,
			fmt.Sprintf("/repos/%s/%s/actions/runners?per_page=100&page=%d", c.cfg.Owner, c.cfg.Repo, page),
			"token "+tok, nil, &resp)
		if err != nil {
			return nil, fmt.Errorf("listing runners: %w", err)
		}
		all = append(all, resp.Runners...)
		if len(all) >= resp.TotalCount || len(resp.Runners) == 0 {
			return all, nil
		}
	}
}

// DeleteRunner removes a runner registration (the no-job teardown path).
// A 404 is success: the runner already removed itself.
func (c *Client) DeleteRunner(ctx context.Context, id int64) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	err = c.doRetry(ctx, http.MethodDelete,
		fmt.Sprintf("/repos/%s/%s/actions/runners/%d", c.cfg.Owner, c.cfg.Repo, id),
		"token "+tok, nil, nil)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string { return fmt.Sprintf("github: HTTP %d: %s", e.code, e.body) }

// retryable: transport errors and 5xx. 4xx are real answers, never retried.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500
	}
	return err != nil
}

func (c *Client) doRetry(ctx context.Context, method, path, auth string, body, out any) error {
	var err error
	for attempt := range 3 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		err = c.do(ctx, method, path, auth, body, out)
		if err == nil || !retryable(err) {
			return err
		}
	}
	return err
}

func (c *Client) do(ctx context.Context, method, path, auth string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBase+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &statusError{code: resp.StatusCode, body: string(b)}
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}
