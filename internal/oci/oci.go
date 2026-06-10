// Package oci pulls tart-format VM images from OCI registries (ADR-0008).
// tart's layout is deliberately non-standard: the bundle's three files ride
// as layers with cirruslabs media types, disk.img split across many
// Apple-LZ4-framed layers that concatenate by uncompressed size. Cilicon
// (MIT) is the reference implementation.
package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

const (
	mediaTypeConfig     = "application/vnd.cirruslabs.tart.config.v1"
	mediaTypeDiskPrefix = "application/vnd.cirruslabs.tart.disk."
	mediaTypeDiskV2     = "application/vnd.cirruslabs.tart.disk.v2"
	mediaTypeNVRAM      = "application/vnd.cirruslabs.tart.nvram.v1"

	annotationUncompressedSize = "org.cirruslabs.tart.uncompressed-size"

	manifestAccept = "application/vnd.oci.image.manifest.v1+json"
)

// Ref is a parsed image reference: host/name[:tag][@sha256:...].
type Ref struct {
	Host string
	Name string
	Tag  string
	// Digest pins the manifest; takes precedence over Tag.
	Digest string
}

// ParseRef parses ghcr.io/cirruslabs/macos-tahoe-xcode:26.3 and the
// @sha256: pinned form.
func ParseRef(s string) (Ref, error) {
	r := Ref{Tag: "latest"}
	if i := strings.Index(s, "@"); i >= 0 {
		r.Digest = s[i+1:]
		s = s[:i]
		if !strings.HasPrefix(r.Digest, "sha256:") {
			return Ref{}, fmt.Errorf("unsupported digest %q", r.Digest)
		}
	}
	if i := strings.LastIndexByte(s, ':'); i > strings.LastIndexByte(s, '/') {
		r.Tag = s[i+1:]
		s = s[:i]
	}
	host, name, ok := strings.Cut(s, "/")
	if !ok || !strings.ContainsAny(host, ".:") {
		return Ref{}, fmt.Errorf("reference %q must include a registry host", s)
	}
	r.Host, r.Name = host, name
	return r, nil
}

func (r Ref) String() string {
	s := r.Host + "/" + r.Name
	if r.Digest != "" {
		return s + "@" + r.Digest
	}
	return s + ":" + r.Tag
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Layers        []descriptor `json:"layers"`
}

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
}

func (d descriptor) uncompressedSize() (int64, error) {
	s, ok := d.Annotations[annotationUncompressedSize]
	if !ok {
		return 0, fmt.Errorf("disk layer %s missing %s annotation", d.Digest, annotationUncompressedSize)
	}
	return strconv.ParseInt(s, 10, 64)
}

// Client pulls tart-format images. Auth is the standard registry token
// dance: 401 challenge → token endpoint → bearer (anonymous pull works for
// public images).
type Client struct {
	hc *http.Client
	// Progress, when set, receives byte deltas as layer data arrives — the
	// feed for ENSURE_IMAGE's stall detector (a slow pull is fine, a silent
	// one is not).
	Progress func(bytes int64)

	mu     sync.Mutex
	tokens map[string]string // host -> bearer token
}

func NewClient() *Client {
	return &Client{hc: &http.Client{Timeout: 0}, tokens: map[string]string{}} // no global timeout: pulls are long; ctx bounds them
}

// Resolve returns the manifest digest for a ref (tag → digest, or the pinned
// digest verified to exist). It takes a bounded.Context because the client's
// transport carries no timeout of its own — an unbounded caller would hang
// forever on a registry that accepts TCP and never answers (ADR-0011).
func (c *Client) Resolve(ctx bounded.Context, ref Ref) (string, error) {
	_, digest, err := c.fetchManifest(ctx, ref)
	return digest, err
}

// Pull downloads the image into destDir as a tart bundle (config.json,
// disk.img, nvram.bin + manifest.json) and returns the manifest digest.
// destDir is created; a partial pull leaves it half-written, so callers pull
// into a temp dir and rename (PullTo does this).
func (c *Client) pull(ctx context.Context, ref Ref, destDir string) (string, error) {
	m, digest, err := c.fetchManifest(ctx, ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}

	var configLayer, nvramLayer *descriptor
	var diskLayers []descriptor
	for i := range m.Layers {
		l := m.Layers[i]
		switch {
		case l.MediaType == mediaTypeConfig:
			configLayer = &m.Layers[i]
		case l.MediaType == mediaTypeNVRAM:
			nvramLayer = &m.Layers[i]
		case strings.HasPrefix(l.MediaType, mediaTypeDiskPrefix):
			if l.MediaType != mediaTypeDiskV2 {
				return "", fmt.Errorf("unsupported disk layer type %s (only disk.v2 supported)", l.MediaType)
			}
			diskLayers = append(diskLayers, l)
		}
	}
	if configLayer == nil || nvramLayer == nil || len(diskLayers) == 0 {
		return "", fmt.Errorf("manifest %s is not a tart image (config=%v nvram=%v disks=%d)",
			digest, configLayer != nil, nvramLayer != nil, len(diskLayers))
	}

	// Small layers: straight blob writes.
	for name, l := range map[string]*descriptor{"config.json": configLayer, "nvram.bin": nvramLayer} {
		if err := c.pullBlobToFile(ctx, ref, *l, filepath.Join(destDir, name)); err != nil {
			return "", err
		}
	}

	// disk.img: reserve the full uncompressed size, then decompress each
	// layer at its running offset, concurrently (cap 4 — ghcr is
	// throughput-bound, more streams don't help).
	diskPath := filepath.Join(destDir, "disk.img")
	var total int64
	offsets := make([]int64, len(diskLayers))
	for i, l := range diskLayers {
		offsets[i] = total
		n, err := l.uncompressedSize()
		if err != nil {
			return "", err
		}
		total += n
	}
	// Refuse a doomed pull up front: the decompressed image must fit. Hours
	// of download ending in ENOSPC is the silent-failure shape this daemon
	// exists to kill (and these images are large — 80GB+ uncompressed).
	if free, err := freeBytes(filepath.Dir(destDir)); err == nil && uint64(total) > free-(2<<30) {
		return "", fmt.Errorf("image %s needs %s uncompressed but only %s is free — refusing to start a pull that cannot complete",
			ref, human(total), human(int64(free)))
	}
	if err := truncateFile(diskPath, total); err != nil {
		return "", err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := range diskLayers {
		g.Go(func() error {
			return c.pullDiskLayer(gctx, ref, diskLayers[i], diskPath, offsets[i])
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}

	raw, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(destDir, "manifest.json"), raw, 0o644); err != nil {
		return "", err
	}
	return digest, nil
}

// PullTo pulls into a sibling temp dir and renames into place, so destDir
// either exists complete or not at all — ENSURE_IMAGE's idempotence depends
// on this. The bounded.Context is typically stall-bounded (Stall.Watch):
// pull duration is unknowable, but silence is not tolerable (ADR-0011).
func (c *Client) PullTo(ctx bounded.Context, ref Ref, destDir string) (string, error) {
	if _, err := os.Stat(filepath.Join(destDir, "manifest.json")); err == nil {
		// Already complete.
		m, digest, err := c.fetchManifest(ctx, ref)
		_ = m
		if err == nil {
			return digest, nil
		}
		return "", err
	}
	tmp := destDir + ".partial"
	_ = os.RemoveAll(tmp)
	digest, err := c.pull(ctx, ref, tmp)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return "", err
	}
	_ = os.RemoveAll(destDir)
	if err := os.Rename(tmp, destDir); err != nil {
		return "", fmt.Errorf("moving pulled image into place: %w", err)
	}
	return digest, nil
}

func (c *Client) fetchManifest(ctx context.Context, ref Ref) (*manifest, string, error) {
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme(ref.Host), ref.Host, ref.Name, target)
	resp, err := c.get(ctx, ref, u, manifestAccept)
	if err != nil {
		return nil, "", fmt.Errorf("fetching manifest %s: %w", ref, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if ref.Digest != "" && ref.Digest != digest {
		return nil, "", fmt.Errorf("manifest digest mismatch: pinned %s, got %s", ref.Digest, digest)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, "", fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, digest, nil
}

func (c *Client) pullBlobToFile(ctx context.Context, ref Ref, d descriptor, path string) error {
	resp, err := c.get(ctx, ref, blobURL(ref, d.Digest), "")
	if err != nil {
		return fmt.Errorf("pulling %s: %w", d.Digest, err)
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h, c.progressWriter()), resp.Body); err != nil {
		return fmt.Errorf("downloading %s: %w", d.Digest, err)
	}
	return verifyDigest(d.Digest, h.Sum(nil))
}

func (c *Client) pullDiskLayer(ctx context.Context, ref Ref, d descriptor, diskPath string, offset int64) error {
	resp, err := c.get(ctx, ref, blobURL(ref, d.Digest), "")
	if err != nil {
		return fmt.Errorf("pulling disk layer %s: %w", d.Digest, err)
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(diskPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	h := sha256.New()
	body := io.TeeReader(io.TeeReader(resp.Body, h), c.progressWriter())
	if _, err := appleLZ4Decode(f, body); err != nil {
		return fmt.Errorf("disk layer %s: %w", d.Digest, err)
	}
	return verifyDigest(d.Digest, h.Sum(nil))
}

// get performs one GET with bearer auth, handling the 401 token challenge.
func (c *Client) get(ctx context.Context, ref Ref, u, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		c.mu.Lock()
		if tok := c.tokens[ref.Host]; tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		c.mu.Unlock()
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			_ = resp.Body.Close()
			if err := c.fetchToken(ctx, ref, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: HTTP %d: %s", u, resp.StatusCode, b)
		}
		return resp, nil
	}
}

// fetchToken handles `WWW-Authenticate: Bearer realm="...",service="...",scope="..."`.
func (c *Client) fetchToken(ctx context.Context, ref Ref, challenge string) error {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("registry challenge missing realm: %q", challenge)
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" {
		scope = "repository:" + ref.Name + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("fetching registry token: %w", err)
	}
	defer resp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.Token == "" {
		return fmt.Errorf("registry token response unusable (HTTP %d): %v", resp.StatusCode, err)
	}
	c.mu.Lock()
	c.tokens[ref.Host] = tok.Token
	c.mu.Unlock()
	return nil
}

func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	h = strings.TrimPrefix(strings.TrimSpace(h), "Bearer ")
	for _, part := range strings.Split(h, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok {
			out[k] = strings.Trim(v, `"`)
		}
	}
	return out
}

func blobURL(ref Ref, digest string) string {
	return fmt.Sprintf("%s://%s/v2/%s/blobs/%s", scheme(ref.Host), ref.Host, ref.Name, digest)
}

// scheme: loopback registries (httptest, local dev) speak plain HTTP — the
// same convention container tooling uses for localhost.
func scheme(host string) string {
	if strings.HasPrefix(host, "127.0.0.1") || strings.HasPrefix(host, "localhost") {
		return "http"
	}
	return "https"
}

func verifyDigest(want string, sum []byte) error {
	got := "sha256:" + hex.EncodeToString(sum)
	if want != got {
		return fmt.Errorf("blob digest mismatch: manifest says %s, downloaded %s", want, got)
	}
	return nil
}

func truncateFile(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

func human(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

type progressWriter struct{ fn func(int64) }

func (p progressWriter) Write(b []byte) (int, error) {
	if p.fn != nil {
		p.fn(int64(len(b)))
	}
	return len(b), nil
}

func (c *Client) progressWriter() io.Writer { return progressWriter{fn: c.Progress} }

// WithHTTPClient overrides the transport (tests; custom CA setups).
func (c *Client) WithHTTPClient(hc *http.Client) *Client {
	c.hc = hc
	return c
}
