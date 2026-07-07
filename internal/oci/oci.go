// Package oci pulls tart-format VM images from OCI registries.
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
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/diskfree"
	"github.com/bojanrajkovic/runny/internal/obs"
	"github.com/bojanrajkovic/runny/internal/tart"
)

// PullHeadroom is the free-space buffer required above the image's uncompressed
// size before a pull is allowed to start. The doctor's disk-headroom check uses
// the same value so its verdict predicts the pull guard.
const PullHeadroom = 2 << 30 // 2 GiB

// DiskHeadroomError is returned by a pull refused at the pre-flight disk guard:
// the uncompressed image plus PullHeadroom does not fit in the destination
// filesystem. It is the one DETERMINISTIC pull failure — it fails identically
// for every concurrent slot until host disk state changes — so a caller can
// errors.As it, poll for headroom against NeedBytes, and re-attempt only once
// there is room, instead of re-running a guaranteed-doomed pull.
type DiskHeadroomError struct {
	Ref        string
	ImageBytes int64 // uncompressed image size (what the message reports)
	FreeBytes  int64 // free space at refusal time
}

func (e *DiskHeadroomError) Error() string {
	return fmt.Sprintf("image %s needs %s uncompressed but only %s is free — refusing to start a pull that cannot complete",
		e.Ref, HumanBytes(e.ImageBytes), HumanBytes(e.FreeBytes))
}

// NeedBytes is the free-space threshold the pull requires: the uncompressed
// image plus PullHeadroom. A headroom poll must compare available space against
// this, not ImageBytes alone (the guard enforces the same sum).
func (e *DiskHeadroomError) NeedBytes() uint64 {
	return uint64(e.ImageBytes) + PullHeadroom
}

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
	Layers []descriptor `json:"layers"`
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
	// one is not). It may be invoked CONCURRENTLY from multiple goroutines:
	// pull fans the disk layers across an errgroup (up to 4 at once), each
	// writing through progressWriter. The callback must be goroutine-safe —
	// the production consumer in internal/images feeds a mutex-guarded
	// progress aggregator and a bounded.Stall, both safe under concurrency.
	Progress func(bytes int64)

	mu     sync.Mutex
	tokens map[string]string // host -> bearer token
}

func NewClient() *Client {
	// No global timeout: pulls are long; ctx bounds them.
	return &Client{hc: &http.Client{Timeout: 0, Transport: &obs.HTTPTransport{}}, tokens: map[string]string{}}
}

// Resolve returns the manifest digest for a ref (tag → digest, or the pinned
// digest verified to exist). It takes a bounded.Context because the client's
// transport carries no timeout of its own — an unbounded caller would hang
// forever on a registry that accepts TCP and never answers.
func (c *Client) Resolve(ctx bounded.Context, ref Ref) (string, error) {
	_, _, digest, err := c.fetchManifest(ctx, ref)
	return digest, err
}

// ResolveWithDiskBytes returns the manifest digest and the total declared
// uncompressed disk size for ref. Fetches the manifest once; use this from
// callers that need both rather than calling Resolve and re-fetching.
func (c *Client) ResolveWithDiskBytes(ctx bounded.Context, ref Ref) (string, int64, error) {
	m, _, digest, err := c.fetchManifest(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	_, _, total, err := diskLayerSizes(m.Layers)
	if err != nil {
		return "", 0, err
	}
	return digest, total, nil
}

// diskLayerSizes validates the disk layers among descs (skipping any
// non-disk layer, e.g. config/nvram) and returns, for the disk layers found
// in manifest order: each one's offset into disk.img (the running total
// before it), its declared uncompressed size, and the grand total. A
// disk-prefixed layer of an unsupported version, or one declaring a
// non-positive or overflowing size, is an error. The annotated sizes decide
// file offsets during pull, so the pre-flight size check
// (ResolveWithDiskBytes) and the pull itself (pull) must validate them
// identically — this is the one place that does.
func diskLayerSizes(descs []descriptor) (offsets, sizes []int64, total int64, err error) {
	for i := range descs {
		l := descs[i]
		if !strings.HasPrefix(l.MediaType, mediaTypeDiskPrefix) {
			continue
		}
		if l.MediaType != mediaTypeDiskV2 {
			return nil, nil, 0, fmt.Errorf("unsupported disk layer type %s (only disk.v2 supported)", l.MediaType)
		}
		n, err := l.uncompressedSize()
		if err != nil {
			return nil, nil, 0, err
		}
		if n <= 0 {
			return nil, nil, 0, fmt.Errorf("disk layer %s declares non-positive uncompressed size %d", l.Digest, n)
		}
		if total > math.MaxInt64-n {
			return nil, nil, 0, fmt.Errorf("disk layer sizes overflow the total image size")
		}
		offsets = append(offsets, total)
		sizes = append(sizes, n)
		total += n
	}
	return offsets, sizes, total, nil
}

// Pull downloads the image into destDir as a tart bundle (config.json,
// disk.img, nvram.bin + manifest.json) and returns the manifest digest.
// destDir is created; a partial pull leaves it half-written, so callers pull
// into a temp dir and rename (PullTo does this).
func (c *Client) pull(ctx context.Context, ref Ref, destDir string) (string, error) {
	m, raw, digest, err := c.fetchManifest(ctx, ref)
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
			diskLayers = append(diskLayers, l)
		}
	}
	if configLayer == nil || nvramLayer == nil || len(diskLayers) == 0 {
		return "", fmt.Errorf("manifest %s is not a tart image (config=%v nvram=%v disks=%d)",
			digest, configLayer != nil, nvramLayer != nil, len(diskLayers))
	}

	// Validate disk layers before downloading anything: an unsupported disk
	// layer type must be refused up front, not after config.json/nvram.bin
	// have already been pulled.
	offsets, sizes, total, err := diskLayerSizes(diskLayers)
	if err != nil {
		return "", err
	}

	// Small layers: straight blob writes.
	for name, l := range map[string]*descriptor{"config.json": configLayer, "nvram.bin": nvramLayer} {
		if err := c.pullBlobToFile(ctx, ref, *l, filepath.Join(destDir, name)); err != nil {
			return "", err
		}
	}

	// disk.img: reserve the full uncompressed size, then decompress each
	// layer at its running offset, concurrently (cap 4 — ghcr is
	// throughput-bound, more streams don't help). The annotated sizes are
	// registry-supplied: validate them here and enforce them during decode —
	// they decide file offsets, so a layer that decodes past its annotation
	// would silently overwrite its neighbor while every digest still checks
	// out (digests cover the compressed stream, not the decoded bytes).
	diskPath := filepath.Join(destDir, "disk.img")
	// Refuse a doomed pull up front: the decompressed image must fit. Hours
	// of download ending in ENOSPC is the silent-failure shape this daemon
	// exists to kill (and these images are large — 80GB+ uncompressed).
	free, err := diskfree.AvailableBytes(filepath.Dir(destDir))
	if err != nil {
		return "", fmt.Errorf("checking free space before pull: %w", err)
	}
	if uint64(total)+PullHeadroom > free {
		return "", &DiskHeadroomError{Ref: ref.String(), ImageBytes: total, FreeBytes: int64(free)}
	}
	if err := truncateFile(diskPath, total); err != nil {
		return "", err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := range diskLayers {
		g.Go(func() error {
			return c.pullDiskLayer(gctx, ref, diskLayers[i], diskPath, offsets[i], sizes[i])
		})
	}
	if err := g.Wait(); err != nil {
		return "", err
	}

	// Store the registry's bytes verbatim: the digest is the hash of these
	// exact bytes (re-marshaling would drop unknown fields and reorder
	// keys), and the cache-hit path recomputes the digest from this file.
	if err := os.WriteFile(filepath.Join(destDir, "manifest.json"), raw, 0o644); err != nil {
		return "", err
	}
	return digest, nil
}

// pullLocks serializes concurrent pulls into the same destination: all slots
// of a pool share one image cache path and enter ENSURE_IMAGE together on a
// cold start. A capacity-1 channel rather than a mutex so the wait is
// ctx-aware (an operator recycle must be able to interrupt a waiting slot).
// Never deleted; bounded by the number of distinct images.
var pullLocks sync.Map // destDir -> chan struct{} (capacity-1 semaphore)

// PullInProgress reports whether a PullTo call for destDir is currently in
// flight. The prune planner uses this to distinguish a live .partial-* temp
// dir (skip it) from an orphan left by a prior crash (safe to delete).
func PullInProgress(destDir string) bool {
	semAny, ok := pullLocks.Load(destDir)
	if !ok {
		return false
	}
	return len(semAny.(chan struct{})) > 0
}

// AcquireSlot serializes concurrent callers on the per-key capacity-1
// semaphore in locks (creating it on first use), returning a release func
// that MUST be called (defer it) to free the slot. An uncontended caller
// proceeds immediately; a contended one blocks on onWait (if non-nil, called
// once, before blocking) and then waits interruptibly until the holder
// releases or ctx ends — a mutex would make that wait both uninterruptible
// (an operator recycle couldn't free a waiter) and invisible (no chance to
// report it). desc names what's contended, for the ctx-cancelled error.
// Exported because internal/images' tarball-download lock is the identical
// pattern as this package's own pullLocks.
func AcquireSlot(ctx context.Context, locks *sync.Map, key, desc string, onWait func()) (func(), error) {
	semAny, _ := locks.LoadOrStore(key, make(chan struct{}, 1))
	sem := semAny.(chan struct{})
	select {
	case sem <- struct{}{}:
	default:
		if onWait != nil {
			onWait()
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for a concurrent %s: %w", desc, context.Cause(ctx))
		}
	}
	return func() { <-sem }, nil
}

// PullTo pulls into a sibling temp dir and renames into place, so destDir
// either exists complete or not at all — ENSURE_IMAGE's idempotence depends
// on this. The bounded.Context is typically stall-bounded (Stall.Watch):
// pull duration is unknowable, but silence is not tolerable.
//
// Concurrent callers for the same destDir serialize on a per-destination
// lock: one pulls, the rest wait and take the cache hit. The wait itself is
// transitively bounded — it ends when the holder's own bounded pull does.
// Without it, two slots raced a shared temp dir: the loser's cleanup could
// delete the winner's freshly placed bundle, or a half-written disk.img
// could be renamed into place and pass Verify forever.
func (c *Client) PullTo(ctx bounded.Context, ref Ref, destDir string) (string, error) {
	release, err := AcquireSlot(ctx, &pullLocks, destDir, "pull into "+destDir, nil)
	if err != nil {
		return "", err
	}
	defer release()

	// A cache hit must pass the same completeness bar the consumer applies
	// (tart.Bundle.Verify), not just "manifest.json exists" — a manifest
	// next to a missing or empty disk.img once wedged the slot in a
	// permanent fail loop with no self-heal: Ensure's Verify rejected the
	// bundle, PullTo kept declaring it complete. The digest is the hash of
	// the manifest bytes stored at pull time — read from disk rather than
	// re-asking the registry: waiters arrive here with their stall budgets
	// already spent waiting for the winner, and a cache hit must not depend
	// on the network.
	if raw, err := os.ReadFile(filepath.Join(destDir, "manifest.json")); err == nil && tart.Bundle(destDir).Verify() == nil {
		sum := sha256.Sum256(raw)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if ref.Digest == "" || ref.Digest == digest {
			return digest, nil
		}
		// On-disk manifest doesn't hash to the pinned digest: treat the
		// bundle as corrupt and fall through to re-pull it.
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(destDir), filepath.Base(destDir)+".partial-")
	if err != nil {
		return "", err
	}
	digest, err := c.pull(ctx, ref, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	_ = os.RemoveAll(destDir)
	if err := os.Rename(tmp, destDir); err != nil {
		_ = os.RemoveAll(tmp)
		return "", fmt.Errorf("moving pulled image into place: %w", err)
	}
	return digest, nil
}

// fetchManifest returns the parsed manifest, the registry's exact bytes
// (what the digest covers), and the digest.
func (c *Client) fetchManifest(ctx context.Context, ref Ref) (*manifest, []byte, string, error) {
	target := ref.Tag
	if ref.Digest != "" {
		target = ref.Digest
	}
	u := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme(ref.Host), ref.Host, ref.Name, target)
	resp, err := c.get(ctx, obs.HTTPRegistryManifest, ref, u, manifestAccept)
	if err != nil {
		return nil, nil, "", fmt.Errorf("fetching manifest %s: %w", ref, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, "", err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if ref.Digest != "" && ref.Digest != digest {
		return nil, nil, "", fmt.Errorf("manifest digest mismatch: pinned %s, got %s", ref.Digest, digest)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, "", fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, body, digest, nil
}

// maxSmallBlobSize caps config.json / nvram.bin downloads. tart's are a few
// KB; 64 MiB is generous headroom. The manifest is the only thing declaring
// blob sizes, so a hostile registry must not get to declare unbounded ones.
const maxSmallBlobSize = 64 << 20

func (c *Client) pullBlobToFile(ctx context.Context, ref Ref, d descriptor, path string) error {
	if d.Size <= 0 || d.Size > maxSmallBlobSize {
		return fmt.Errorf("blob %s declares implausible size %d", d.Digest, d.Size)
	}
	resp, err := c.get(ctx, obs.HTTPRegistryBlob, ref, blobURL(ref, d.Digest), "")
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
	// Read at most the declared size: an endless body must fail here, not
	// after it has filled the disk. Stall detection cannot catch it (bytes
	// keep arriving) and digest verification would only fire afterwards.
	n, err := io.Copy(io.MultiWriter(f, h, c.progressWriter()), io.LimitReader(resp.Body, d.Size))
	if err != nil {
		return fmt.Errorf("downloading %s: %w", d.Digest, err)
	}
	if n != d.Size {
		return fmt.Errorf("blob %s: got %d bytes, manifest says %d", d.Digest, n, d.Size)
	}
	// Close errors are write errors: a flush that fails must fail the pull,
	// not leave a short file behind a passing digest (the digest hashed the
	// stream, not the file). The deferred Close above only backstops early
	// returns; double-closing an os.File is harmless.
	if err := f.Close(); err != nil {
		return fmt.Errorf("flushing %s: %w", path, err)
	}
	return verifyDigest(d.Digest, h.Sum(nil))
}

func (c *Client) pullDiskLayer(ctx context.Context, ref Ref, d descriptor, diskPath string, offset, expected int64) error {
	if d.Size <= 0 {
		return fmt.Errorf("disk layer %s declares non-positive size %d", d.Digest, d.Size)
	}
	resp, err := c.get(ctx, obs.HTTPRegistryBlob, ref, blobURL(ref, d.Digest), "")
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
	// Bound both directions of the decode: read at most the declared
	// compressed size, write at most the annotated uncompressed size, and
	// demand the output land exactly on the annotation — it determined this
	// layer's slot in disk.img, and the digest (which covers the compressed
	// stream, not the decoded bytes) cannot catch an overrun into the
	// neighboring layer or a decode that quietly fell short.
	body := io.TeeReader(io.LimitReader(resp.Body, d.Size), io.MultiWriter(h, c.progressWriter()))
	written, err := appleLZ4Decode(&boundedWriter{w: f, remain: expected}, body)
	if err != nil {
		return fmt.Errorf("disk layer %s: %w", d.Digest, err)
	}
	if written != expected {
		return fmt.Errorf("disk layer %s decoded to %d bytes, annotation says %d", d.Digest, written, expected)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("flushing disk layer %s: %w", d.Digest, err)
	}
	return verifyDigest(d.Digest, h.Sum(nil))
}

// errDecodeOverrun reports a layer decoding past its annotated size.
var errDecodeOverrun = errors.New("layer decodes past its declared uncompressed size")

// boundedWriter refuses writes past its budget — the guard that turns a
// decompression bomb into an immediate error instead of a full disk or a
// silently corrupted neighbor layer.
type boundedWriter struct {
	w      io.Writer
	remain int64
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > b.remain {
		return 0, errDecodeOverrun
	}
	n, err := b.w.Write(p)
	b.remain -= int64(n)
	return n, err
}

// get performs one GET with bearer auth, handling the 401 token challenge.
// Every caller states the round trip's class here — a new call site cannot
// compile without one.
func (c *Client) get(ctx context.Context, class obs.HTTPClass, ref Ref, u, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(obs.WithHTTPClass(ctx, class), http.MethodGet, u, nil)
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
	ctx = obs.WithHTTPClass(ctx, obs.HTTPRegistryToken)
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return fmt.Errorf("registry challenge missing realm: %q", challenge)
	}
	// The realm is registry-controlled and the minted token flows back in
	// our requests: refuse to chase it over plaintext (loopback excepted,
	// matching the registry scheme convention above).
	if u, err := url.Parse(realm); err != nil {
		return fmt.Errorf("registry challenge realm %q: %w", realm, err)
	} else if u.Scheme != "https" && !isLoopbackHost(u.Host) {
		return fmt.Errorf("registry challenge realm %q is not https", realm)
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
// same convention container tooling uses for localhost. The check is exact,
// not a prefix match: localhost.evil.com and 127.0.0.10 must not earn a
// plaintext downgrade.
func scheme(host string) string {
	if isLoopbackHost(host) {
		return "http"
	}
	return "https"
}

func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

// ShortDigest is the one display convention for digests: strip a leading
// "sha256:" and keep the first 12 hex characters (shorter input is returned
// as-is). Exported so every caller that prints a digest for a human — the
// prune planner included — renders it the same way.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		d = d[:12]
	}
	return d
}

// HumanBytes renders a byte count for humans. Exported because the pull
// progress reporting in internal/images speaks the same dialect — two
// copies of this had already drifted apart once.
func HumanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// ProgressWriter adapts a byte-delta callback to io.Writer for a TeeReader.
// Fn holds no shared state of its own, but a caller may run several of these
// concurrently (pull's errgroup fans one per disk layer), so Fn must be
// goroutine-safe — see Client.Progress for the contract this client's own
// callback honors. Exported because internal/images' runner-tarball download
// wants the identical adapter; two copies of this had already drifted apart
// once (see HumanBytes).
type ProgressWriter struct{ Fn func(int64) }

func (p ProgressWriter) Write(b []byte) (int, error) {
	if p.Fn != nil {
		p.Fn(int64(len(b)))
	}
	return len(b), nil
}

func (c *Client) progressWriter() io.Writer { return ProgressWriter{Fn: c.Progress} }
