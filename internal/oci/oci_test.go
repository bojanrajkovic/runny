package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// testCtx satisfies the bounded.Context the pull API demands (ADR-0011).
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
		SchemaVersion: 2,
		MediaType:     manifestAccept,
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

// A waiter behind another slot's pull of the same destination must (a) signal
// Waiting so its stall detector treats the wait as progress, not silence, and
// (b) proceed normally once the holder releases. Regression: a second slot in
// ENSURE_IMAGE showed STALLED while the first pulled their shared image.
func TestPullToWaitingCallback(t *testing.T) {
	config := []byte(`{"os":"darwin"}`)
	f := newFakeRegistry(t, config, []byte("nvram"), []byte("disk"))
	_, ref := f.start()
	dest := filepath.Join(t.TempDir(), "bundle")

	// Occupy the per-dest semaphore, simulating a pull in flight.
	semAny, _ := pullLocks.LoadOrStore(dest, make(chan struct{}, 1))
	sem := semAny.(chan struct{})
	sem <- struct{}{}

	waited := make(chan struct{})
	stopped := make(chan struct{})
	c := NewClient()
	c.Waiting = func() func() {
		close(waited)
		return func() { close(stopped) }
	}

	got := make(chan error, 1)
	go func() {
		_, err := c.PullTo(testCtx(t), ref, dest)
		got <- err
	}()

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("Waiting callback never fired for a contended pull")
	}
	<-sem // release the "holder"
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("PullTo after wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PullTo did not complete after the holder released")
	}
	select {
	case <-stopped:
	default:
		t.Error("Waiting's stop func was not called after the wait ended")
	}
}

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
