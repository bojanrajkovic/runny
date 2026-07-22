package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/obs"
)

// testCtx satisfies the bounded.Context the pull API demands.
func testCtx(t *testing.T) bounded.Context {
	t.Helper()
	ctx, cancel := bounded.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// appleLZ4Encode is a test-only encoder for Apple's LZ4 block framing,
// mirroring what tart's Compression-framework output looks like.
func appleLZ4Encode(t *testing.T, data []byte, blockSize int) []byte {
	t.Helper()
	var out bytes.Buffer
	for off := 0; off < len(data); off += blockSize {
		end := min(off+blockSize, len(data))
		chunk := data[off:end]
		comp := make([]byte, lz4.CompressBlockBound(len(chunk)))
		n, err := lz4.CompressBlock(chunk, comp, nil)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 || n >= len(chunk) {
			// Incompressible: raw block.
			out.Write([]byte("bv4-"))
			_ = binary.Write(&out, binary.LittleEndian, uint32(len(chunk)))
			out.Write(chunk)
			continue
		}
		out.Write([]byte("bv41"))
		_ = binary.Write(&out, binary.LittleEndian, uint32(len(chunk)))
		_ = binary.Write(&out, binary.LittleEndian, uint32(n))
		out.Write(comp[:n])
	}
	out.Write([]byte("bv4$"))
	return out.Bytes()
}

func TestAppleLZ4RoundTrip(t *testing.T) {
	// Compressible data (repeated) + incompressible (random) across blocks.
	data := append(bytes.Repeat([]byte("runny runny runny "), 10_000), randomBytes(64*1024)...)
	enc := appleLZ4Encode(t, data, 32*1024)
	var dec bytes.Buffer
	n, err := appleLZ4Decode(&dec, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != int64(len(data)) || !bytes.Equal(dec.Bytes(), data) {
		t.Fatalf("round-trip mismatch: %d bytes out, want %d", n, len(data))
	}
}

func TestAppleLZ4RejectsGarbage(t *testing.T) {
	if _, err := appleLZ4Decode(io.Discard, strings.NewReader("XXXXgarbage")); err == nil {
		t.Fatal("want unknown-magic error")
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(42))
	_, _ = r.Read(b)
	return b
}

func TestShortDigest(t *testing.T) {
	cases := map[string]string{
		"sha256:aabbccddeeff00112233": "aabbccddeeff",
		"sha256:aabbcc":               "aabbcc",       // shorter than 12: returned as-is
		"aabbccddeeff00112233":        "aabbccddeeff", // no prefix, still truncated
		"":                            "",
	}
	for in, want := range cases {
		if got := ShortDigest(in); got != want {
			t.Errorf("ShortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in   string
		want Ref
		err  bool
	}{
		{in: "ghcr.io/cirruslabs/macos-tahoe-xcode:26.3", want: Ref{Host: "ghcr.io", Name: "cirruslabs/macos-tahoe-xcode", Tag: "26.3"}},
		{in: "ghcr.io/cirruslabs/x", want: Ref{Host: "ghcr.io", Name: "cirruslabs/x", Tag: "latest"}},
		{in: "ghcr.io/a/b@sha256:abc", want: Ref{Host: "ghcr.io", Name: "a/b", Tag: "latest", Digest: "sha256:abc"}},
		{in: "no-registry-host", err: true},
		{in: "ghcr.io/a/b@md5:nope", err: true},
	}
	for _, c := range cases {
		got, err := ParseRef(c.in)
		if c.err != (err != nil) {
			t.Errorf("ParseRef(%q) err = %v", c.in, err)
			continue
		}
		if !c.err && got != c.want {
			t.Errorf("ParseRef(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

// fakeRegistry serves a complete tart-format image with the token-challenge
// auth dance.
type fakeRegistry struct {
	t        *testing.T
	blobs    map[string][]byte // digest -> content
	manifest []byte
	digest   string
}

func newFakeRegistry(t *testing.T, config, nvram, disk []byte) *fakeRegistry {
	t.Helper()
	f := &fakeRegistry{t: t, blobs: map[string][]byte{}}

	add := func(b []byte) string {
		sum := sha256.Sum256(b)
		d := "sha256:" + hex.EncodeToString(sum[:])
		f.blobs[d] = b
		return d
	}

	// Two disk layers, split mid-stream, each Apple-LZ4 encoded.
	half := len(disk) / 2
	l1, l2 := appleLZ4Encode(t, disk[:half], 16*1024), appleLZ4Encode(t, disk[half:], 16*1024)

	m := manifest{
		Layers: []descriptor{
			{MediaType: mediaTypeConfig, Digest: add(config), Size: int64(len(config))},
			{
				MediaType: mediaTypeDiskV2, Digest: add(l1), Size: int64(len(l1)),
				Annotations: map[string]string{annotationUncompressedSize: fmt.Sprint(half)},
			},
			{
				MediaType: mediaTypeDiskV2, Digest: add(l2), Size: int64(len(l2)),
				Annotations: map[string]string{annotationUncompressedSize: fmt.Sprint(len(disk) - half)},
			},
			{MediaType: mediaTypeNVRAM, Digest: add(nvram), Size: int64(len(nvram))},
		},
	}
	f.manifest, _ = json.Marshal(m)
	sum := sha256.Sum256(f.manifest)
	f.digest = "sha256:" + hex.EncodeToString(sum[:])
	return f
}

func (f *fakeRegistry) start() (*httptest.Server, Ref) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("scope"), "test/image") {
			http.Error(w, "bad scope", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anon-token"})
	})
	requireAuth := func(w http.ResponseWriter, r *http.Request, srvURL string) bool {
		if r.Header.Get("Authorization") == "Bearer anon-token" {
			return true
		}
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer realm=%q,service="fake"`, srvURL+"/token"))
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	var srvURL string
	mux.HandleFunc("GET /v2/test/image/manifests/{ref}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r, srvURL) {
			return
		}
		w.Header().Set("Content-Type", manifestAccept)
		_, _ = w.Write(f.manifest)
	})
	mux.HandleFunc("GET /v2/test/image/blobs/{digest}", func(w http.ResponseWriter, r *http.Request) {
		if !requireAuth(w, r, srvURL) {
			return
		}
		b, ok := f.blobs[r.PathValue("digest")]
		if !ok {
			http.Error(w, "no such blob", http.StatusNotFound)
			return
		}
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	srvURL = srv.URL
	f.t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return srv, Ref{Host: host, Name: "test/image", Tag: "26.3"}
}

func TestPullToAssemblesBundle(t *testing.T) {
	config := []byte(`{"os":"darwin"}`)
	nvram := randomBytes(2048)
	disk := append(bytes.Repeat([]byte("DISKDATA"), 50_000), randomBytes(100_000)...)
	f := newFakeRegistry(t, config, nvram, disk)
	_, ref := f.start()

	// progressWriter fans across pull's errgroup, so this callback runs from
	// several goroutines at once (the contract on Client.Progress) — count
	// atomically, or -race flags the bare += as a data race.
	var progressed atomic.Int64
	c := NewClient()
	c.Progress = func(n int64) { progressed.Add(n) }

	dest := filepath.Join(t.TempDir(), "bundle")
	digest, err := c.PullTo(testCtx(t), ref, dest)
	if err != nil {
		t.Fatalf("PullTo: %v", err)
	}
	if digest != f.digest {
		t.Errorf("digest = %s, want %s", digest, f.digest)
	}
	if progressed.Load() == 0 {
		t.Error("progress callback never fired")
	}

	gotDisk, err := os.ReadFile(filepath.Join(dest, "disk.img"))
	if err != nil || !bytes.Equal(gotDisk, disk) {
		t.Fatalf("disk.img mismatch: %d bytes vs %d, err %v", len(gotDisk), len(disk), err)
	}
	gotConfig, _ := os.ReadFile(filepath.Join(dest, "config.json"))
	if !bytes.Equal(gotConfig, config) {
		t.Error("config.json mismatch")
	}
	gotNVRAM, _ := os.ReadFile(filepath.Join(dest, "nvram.bin"))
	if !bytes.Equal(gotNVRAM, nvram) {
		t.Error("nvram.bin mismatch")
	}

	// Second call: cache hit, returns the digest without re-pulling.
	digest2, err := c.PullTo(testCtx(t), ref, dest)
	if err != nil || digest2 != digest {
		t.Errorf("idempotent PullTo: %s, %v", digest2, err)
	}
}

func TestPullDetectsCorruptBlob(t *testing.T) {
	config := []byte(`{"os":"darwin"}`)
	f := newFakeRegistry(t, config, []byte("nvram"), bytes.Repeat([]byte("D"), 4096))
	// Corrupt one blob after manifest digests were computed.
	for d, b := range f.blobs {
		if len(b) == len(config) {
			f.blobs[d] = []byte(`{"os":"tampered"}`)
		}
	}
	_, ref := f.start()
	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("want digest mismatch, got %v", err)
	}
}

func TestPinnedDigestMismatch(t *testing.T) {
	f := newFakeRegistry(t, []byte("{}"), []byte("n"), []byte("d"))
	_, ref := f.start()
	ref.Digest = "sha256:" + strings.Repeat("0", 64)
	_, err := NewClient().Resolve(testCtx(t), ref)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("want pinned-digest mismatch, got %v", err)
	}
}

// All slots of a pool share one cache path and enter ENSURE_IMAGE together
// on a cold start. Concurrent PullTo calls for the same destination must
// serialize: every caller gets the same digest and an intact bundle —
// regression for a shared-temp-dir race where the loser's cleanup could
// delete the winner's bundle or rename a half-written disk.img into place.
func TestPullToConcurrentSameDest(t *testing.T) {
	config := []byte(`{"os":"darwin"}`)
	disk := append(bytes.Repeat([]byte("DISKDATA"), 20_000), randomBytes(50_000)...)
	f := newFakeRegistry(t, config, []byte("nvram"), disk)
	_, ref := f.start()

	dest := filepath.Join(t.TempDir(), "bundle")
	const callers = 4
	digests := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digests[i], errs[i] = NewClient().PullTo(testCtx(t), ref, dest)
		}()
	}
	wg.Wait()

	for i := range callers {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if digests[i] != f.digest {
			t.Fatalf("caller %d digest = %s, want %s", i, digests[i], f.digest)
		}
	}
	gotDisk, err := os.ReadFile(filepath.Join(dest, "disk.img"))
	if err != nil || !bytes.Equal(gotDisk, disk) {
		t.Fatalf("disk.img corrupted by concurrent pulls: %d bytes vs %d, err %v", len(gotDisk), len(disk), err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(t.TempDir(), "*partial*")); len(leftovers) > 0 {
		t.Errorf("temp dirs left behind: %v", leftovers)
	}
}

// The plaintext-HTTP convention is loopback-exact: lookalike hosts must not
// earn a downgrade.
func TestSchemeLoopbackExact(t *testing.T) {
	for host, want := range map[string]string{
		"127.0.0.1":             "http",
		"127.0.0.1:5000":        "http",
		"localhost:5000":        "http",
		"[::1]:5000":            "http",
		"127.0.0.10:5000":       "http", // still loopback: 127.0.0.0/8
		"localhost.evil.com":    "https",
		"127.0.0.1.evil.com":    "https",
		"ghcr.io":               "https",
		"localhost-registry.io": "https",
	} {
		if got := scheme(host); got != want {
			t.Errorf("scheme(%q) = %s, want %s", host, got, want)
		}
	}
}

// A cached bundle must pass the consumer's completeness bar, not just have a
// manifest.json — a manifest next to a missing disk.img once wedged the slot
// in a permanent fail loop (cache declared complete, Verify rejected it).
func TestPullToHealsCorruptCache(t *testing.T) {
	config := []byte(`{"os":"darwin"}`)
	disk := bytes.Repeat([]byte("D"), 8192)
	f := newFakeRegistry(t, config, []byte("nvram"), disk)
	_, ref := f.start()

	dest := filepath.Join(t.TempDir(), "bundle")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	// A corrupt cache: manifest present, disk.img missing.
	if err := os.WriteFile(filepath.Join(dest, "manifest.json"), f.manifest, 0o600); err != nil {
		t.Fatal(err)
	}

	digest, err := NewClient().PullTo(testCtx(t), ref, dest)
	if err != nil {
		t.Fatalf("PullTo did not heal the corrupt cache: %v", err)
	}
	if digest != f.digest {
		t.Errorf("digest = %s, want %s", digest, f.digest)
	}
	gotDisk, err := os.ReadFile(filepath.Join(dest, "disk.img"))
	if err != nil || !bytes.Equal(gotDisk, disk) {
		t.Fatalf("disk.img not re-pulled: %d bytes, err %v", len(gotDisk), err)
	}
}

// tamperManifest re-marshals the registry's manifest after mut edits it.
// Refs in these tests are tag-addressed, so no digest pin breaks.
func (f *fakeRegistry) tamperManifest(t *testing.T, mut func(*manifest)) {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(f.manifest, &m); err != nil {
		t.Fatal(err)
	}
	mut(&m)
	f.manifest, _ = json.Marshal(m)
}

// A layer that decodes past its annotated uncompressed size is a
// decompression bomb and an overwrite of the neighboring layer's region —
// and the blob digest cannot catch it (it covers the compressed stream).
// The decode must refuse at the annotation boundary.
func TestPullRejectsBombLayer(t *testing.T) {
	disk := bytes.Repeat([]byte("D"), 8192)
	f := newFakeRegistry(t, []byte("{}"), []byte("n"), disk)
	f.tamperManifest(t, func(m *manifest) {
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaTypeDiskV2 {
				m.Layers[i].Annotations[annotationUncompressedSize] = "1024" // truly 4096
				return
			}
		}
	})
	_, ref := f.start()
	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), "declared uncompressed size") {
		t.Fatalf("want over-decode rejection, got %v", err)
	}
}

// A layer that decodes SHORT of its annotation leaves a hole of zeros in
// disk.img that every digest check still passes — it must fail the pull.
func TestPullRejectsShortDecode(t *testing.T) {
	disk := bytes.Repeat([]byte("D"), 8192)
	f := newFakeRegistry(t, []byte("{}"), []byte("n"), disk)
	f.tamperManifest(t, func(m *manifest) {
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaTypeDiskV2 {
				m.Layers[i].Annotations[annotationUncompressedSize] = "9000" // truly 4096
				return
			}
		}
	})
	_, ref := f.start()
	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), "annotation says") {
		t.Fatalf("want short-decode rejection, got %v", err)
	}
}

func TestPullRejectsImplausibleBlobSize(t *testing.T) {
	f := newFakeRegistry(t, []byte(`{"os":"darwin"}`), []byte("n"), bytes.Repeat([]byte("D"), 4096))
	f.tamperManifest(t, func(m *manifest) {
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaTypeConfig {
				m.Layers[i].Size = 0
				return
			}
		}
	})
	_, ref := f.start()
	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), "implausible size") {
		t.Fatalf("want implausible-size rejection, got %v", err)
	}
}

func TestPullRejectsNegativeUncompressedSize(t *testing.T) {
	f := newFakeRegistry(t, []byte("{}"), []byte("n"), bytes.Repeat([]byte("D"), 4096))
	f.tamperManifest(t, func(m *manifest) {
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaTypeDiskV2 {
				m.Layers[i].Annotations[annotationUncompressedSize] = "-1"
				return
			}
		}
	})
	_, ref := f.start()
	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "b"))
	if err == nil || !strings.Contains(err.Error(), "non-positive uncompressed size") {
		t.Fatalf("want non-positive-size rejection, got %v", err)
	}
}

// Silence unused-import lint in case of build-tag pruning.
var _ = url.Values{}

// The lock wait must be interruptible: a cancelled context (operator recycle,
// daemon shutdown) frees the waiter instead of leaving it blocked until the
// holder finishes a multi-GiB pull.
func TestResolveWithDiskBytes(t *testing.T) {
	disk := bytes.Repeat([]byte("D"), 8192)
	f := newFakeRegistry(t, []byte(`{"os":"darwin"}`), []byte("nvram"), disk)
	_, ref := f.start()

	digest, total, err := NewClient().ResolveWithDiskBytes(testCtx(t), ref)
	if err != nil {
		t.Fatalf("ResolveWithDiskBytes: %v", err)
	}
	if digest == "" {
		t.Error("ResolveWithDiskBytes returned empty digest")
	}
	if total != int64(len(disk)) {
		t.Errorf("total disk bytes = %d, want %d (len(disk))", total, len(disk))
	}

	// A disk.v1 layer must be rejected — ResolveWithDiskBytes must match pull()'s
	// unsupported-format rejection so the doctor doesn't report a size for images
	// that can't be pulled.
	f2 := newFakeRegistry(t, []byte("{}"), []byte("n"), disk)
	f2.tamperManifest(t, func(m *manifest) {
		for i := range m.Layers {
			if m.Layers[i].MediaType == mediaTypeDiskV2 {
				m.Layers[i].MediaType = "application/vnd.cirruslabs.tart.disk.v1"
				return
			}
		}
	})
	_, ref2 := f2.start()
	_, _, err = NewClient().ResolveWithDiskBytes(testCtx(t), ref2)
	if err == nil || !strings.Contains(err.Error(), "unsupported disk layer type") {
		t.Fatalf("disk.v1 layer: want unsupported-disk-layer-type error, got %v", err)
	}
}

// inflateDeclaredDiskSize rewrites the disk layers' uncompressed-size
// annotation to force the pre-flight disk guard without changing the (small)
// blob bytes: the first disk layer declares `total`, the rest declare 1.
func (f *fakeRegistry) inflateDeclaredDiskSize(t *testing.T, total int64) {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(f.manifest, &m); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	first := true
	for i := range m.Layers {
		if m.Layers[i].MediaType != mediaTypeDiskV2 {
			continue
		}
		if first {
			m.Layers[i].Annotations[annotationUncompressedSize] = fmt.Sprint(total)
			first = false
		} else {
			m.Layers[i].Annotations[annotationUncompressedSize] = "1"
		}
	}
	f.manifest, _ = json.Marshal(m)
	sum := sha256.Sum256(f.manifest)
	f.digest = "sha256:" + hex.EncodeToString(sum[:])
}

// The pre-flight disk guard must surface a typed *DiskHeadroomError so the
// shared image puller can errors.As it, classify the failure as deterministic,
// and poll for headroom against NeedBytes rather than re-running a doomed pull.
func TestDiskGuardReturnsTypedError(t *testing.T) {
	f := newFakeRegistry(t, []byte(`{"os":"darwin"}`), []byte("nvram"), bytes.Repeat([]byte("D"), 4096))
	const exabyte = 1 << 60 // far exceeds any test host's free space
	f.inflateDeclaredDiskSize(t, exabyte)
	_, ref := f.start()

	_, err := NewClient().PullTo(testCtx(t), ref, filepath.Join(t.TempDir(), "bundle"))
	var dh *DiskHeadroomError
	if !errors.As(err, &dh) {
		t.Fatalf("want *DiskHeadroomError, got %T: %v", err, err)
	}
	if dh.ImageBytes < exabyte {
		t.Errorf("ImageBytes = %d, want >= %d", dh.ImageBytes, int64(exabyte))
	}
	if dh.NeedBytes() != uint64(dh.ImageBytes)+RequiredHeadroom(dh.ImageBytes) {
		t.Errorf("NeedBytes() = %d, want ImageBytes+RequiredHeadroom = %d", dh.NeedBytes(), uint64(dh.ImageBytes)+RequiredHeadroom(dh.ImageBytes))
	}
}

// The puller reads the disk error after it crosses Ensure's stallErr wrapping
// (fmt.Errorf("...: %w", err)); errors.As must still recover the typed error.
func TestDiskHeadroomErrorSurvivesWrapping(t *testing.T) {
	base := &DiskHeadroomError{Ref: "ghcr.io/x/y:1", ImageBytes: 100 << 30, FreeBytes: 1 << 30}
	wrapped := fmt.Errorf("pulling ghcr.io/x/y:1: %w", base)
	var dh *DiskHeadroomError
	if !errors.As(wrapped, &dh) {
		t.Fatalf("errors.As did not recover *DiskHeadroomError through %%w wrapping")
	}
	if dh.NeedBytes() != uint64(100<<30)+RequiredHeadroom(100<<30) {
		t.Errorf("NeedBytes() = %d after unwrap, want %d", dh.NeedBytes(), uint64(100<<30)+RequiredHeadroom(100<<30))
	}
}

func TestPullToWaitInterruptible(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "bundle")
	semAny, _ := pullLocks.LoadOrStore(dest, make(chan struct{}, 1))
	sem := semAny.(chan struct{})
	sem <- struct{}{}
	defer func() { <-sem }()

	ctx, cancel := bounded.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := NewClient().PullTo(ctx, Ref{Host: "example.com", Name: "x", Tag: "y"}, dest)
	if err == nil || !strings.Contains(err.Error(), "waiting for a concurrent pull") {
		t.Fatalf("want interruptible-wait error, got %v", err)
	}
}

// TestPullInProgress: reports true while a pull holds the semaphore, false
// once released.
func TestPullInProgress(t *testing.T) {
	dest := t.TempDir()
	if PullInProgress(dest) {
		t.Fatal("PullInProgress returned true before any lock acquired")
	}
	sem := make(chan struct{}, 1)
	pullLocks.Store(dest, sem)
	t.Cleanup(func() { pullLocks.Delete(dest) })

	if PullInProgress(dest) {
		t.Fatal("PullInProgress returned true with semaphore empty (no pull in flight)")
	}
	sem <- struct{}{} // simulate pull acquiring the lock
	if !PullInProgress(dest) {
		t.Fatal("PullInProgress returned false while semaphore is held")
	}
	<-sem // release
	if PullInProgress(dest) {
		t.Fatal("PullInProgress returned true after semaphore released")
	}
}

// scopedCtx returns a bounded context carrying an obs cycle scope inside an
// ENSURE_IMAGE step, and a snapshot accessor for the captured events. The
// capture is locked: PullTo fans blob GETs across an errgroup, so a scoped
// pull emits KindHTTP events from several goroutines at once.
func scopedCtx(t *testing.T) (bounded.Context, func() []obs.Event) {
	t.Helper()
	var mu sync.Mutex
	var events []obs.Event
	emit := func(e obs.Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, e)
	}
	base := obs.WithStep(obs.WithCycle(t.Context(), emit,
		obs.CycleRef{Slot: "slot-0", CycleID: "cafe"}), "ENSURE_IMAGE")
	ctx, cancel := bounded.WithTimeout(base, time.Minute)
	t.Cleanup(cancel)
	return ctx, func() []obs.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]obs.Event(nil), events...)
	}
}

// A scoped Resolve narrates the whole auth dance, each round trip with its
// own class: the anonymous manifest GET answered 401, the token fetch, the
// authenticated retry — nothing classed "other", nothing invisible.
func TestResolveEmitsClassedHTTPEvents(t *testing.T) {
	f := newFakeRegistry(t, []byte(`{"os":"darwin"}`), []byte{1}, bytes.Repeat([]byte{7}, 4096))
	srv, ref := f.start()
	defer srv.Close()

	ctx, captured := scopedCtx(t)

	if _, err := NewClient().Resolve(ctx, ref); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, e := range captured() {
		if e.Kind == obs.KindHTTP {
			got = append(got, fmt.Sprintf("%s:%d", e.HTTP.Class, e.HTTP.Status))
		}
	}
	want := []string{
		string(obs.HTTPRegistryManifest) + ":401",
		string(obs.HTTPRegistryToken) + ":200",
		string(obs.HTTPRegistryManifest) + ":200",
	}
	if !slices.Equal(got, want) {
		t.Errorf("round trips = %v, want %v", got, want)
	}
}

// Blob GETs carry the blob class when the context is scoped. (The production
// pull actor's context carries no scope, so its blob traffic emits nothing —
// the transport's passthrough contract, tested in internal/obs.)
func TestPullToEmitsBlobClass(t *testing.T) {
	f := newFakeRegistry(t, []byte(`{"os":"darwin"}`), []byte{1}, bytes.Repeat([]byte{7}, 4096))
	srv, ref := f.start()
	defer srv.Close()

	ctx, captured := scopedCtx(t)

	if _, err := NewClient().PullTo(ctx, ref, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	blobs := 0
	for _, e := range captured() {
		if e.Kind != obs.KindHTTP {
			continue
		}
		if e.HTTP.Class == obs.HTTPOther {
			t.Errorf("a registry round trip fell through to %q", obs.HTTPOther)
		}
		if e.HTTP.Class == obs.HTTPRegistryBlob && e.HTTP.Status == http.StatusOK {
			blobs++
		}
	}
	if blobs != 4 { // config + two disk layers + nvram
		t.Errorf("got %d ok blob round trips, want 4", blobs)
	}
}
