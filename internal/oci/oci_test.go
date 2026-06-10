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

	var progressed int64
	c := NewClient()
	c.Progress = func(n int64) { progressed += n }

	dest := filepath.Join(t.TempDir(), "bundle")
	digest, err := c.PullTo(testCtx(t), ref, dest)
	if err != nil {
		t.Fatalf("PullTo: %v", err)
	}
	if digest != f.digest {
		t.Errorf("digest = %s, want %s", digest, f.digest)
	}
	if progressed == 0 {
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

// Silence unused-import lint in case of build-tag pruning.
var _ = url.Values{}
