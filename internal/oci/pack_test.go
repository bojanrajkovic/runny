package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// TestWriteImageProducesAWellFormedOCILayout checks WriteImage's output
// against the OCI Image Layout spec independent of runny's own reader --
// oci-layout content, index.json shape, blob content-addressing, and
// manifest/config/layers shape -- so a generic tool (oras cp
// --from-oci-layout, skopeo, umoci) can consume it, even though nothing in
// this package's own pull path checks most of these details. The round-trip
// tests in oci_test.go cover the complementary claim: that runny's own
// reader accepts it and reproduces the original bytes.
func TestWriteImageProducesAWellFormedOCILayout(t *testing.T) {
	cfg := tart.Config{OS: "windows", Arch: "amd64", CPUCount: 2, MemorySize: 4 << 30}
	disk := bytes.Repeat([]byte("OCILAYOUT"), 10_000)
	packed, err := WriteImage(t.TempDir(), cfg, bytes.NewReader(disk), int64(len(disk)), []byte{0})
	if err != nil {
		t.Fatalf("WriteImage: %v", err)
	}

	// oci-layout: the marker file every OCI Image Layout tool checks first.
	layoutBytes, err := os.ReadFile(filepath.Join(packed.Dir, "oci-layout"))
	if err != nil {
		t.Fatal(err)
	}
	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := json.Unmarshal(layoutBytes, &layout); err != nil {
		t.Fatalf("oci-layout is not valid JSON: %v", err)
	}
	if layout.ImageLayoutVersion != "1.0.0" {
		t.Errorf("imageLayoutVersion = %q, want 1.0.0", layout.ImageLayoutVersion)
	}

	// Every blob file must be named by the hex of its own sha256 digest --
	// content-addressing is the property the whole format leans on; a
	// mismatch here means no OCI tool could ever locate a blob it just
	// verified.
	blobsDir := filepath.Join(packed.Dir, "blobs", "sha256")
	entries, err := os.ReadDir(blobsDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no blobs written")
	}
	blobs := map[string][]byte{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(blobsDir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if got := hex.EncodeToString(sum[:]); got != e.Name() {
			t.Errorf("blob file %s actually hashes to %s", e.Name(), got)
		}
		blobs["sha256:"+e.Name()] = b
	}

	// index.json: schemaVersion 2, exactly one manifest entry, referencing a
	// blob that's actually present.
	indexBytes, err := os.ReadFile(filepath.Join(packed.Dir, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index struct {
		SchemaVersion int          `json:"schemaVersion"`
		Manifests     []descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		t.Fatalf("index.json is not valid JSON: %v", err)
	}
	if index.SchemaVersion != 2 {
		t.Errorf("index.json schemaVersion = %d, want 2", index.SchemaVersion)
	}
	if len(index.Manifests) != 1 || index.Manifests[0].Digest != packed.Digest {
		t.Fatalf("index.json manifests = %+v, want exactly [%s]", index.Manifests, packed.Digest)
	}

	// The manifest itself: schemaVersion 2, the OCI manifest media type, a
	// config descriptor resolving to a real (JSON) blob, and every layer
	// descriptor resolving to a real blob whose size matches the descriptor.
	manifestBytes, ok := blobs[packed.Digest]
	if !ok {
		t.Fatalf("manifest digest %s has no corresponding blob", packed.Digest)
	}
	var m struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.SchemaVersion != 2 {
		t.Errorf("manifest schemaVersion = %d, want 2", m.SchemaVersion)
	}
	if m.MediaType != manifestAccept {
		t.Errorf("manifest mediaType = %q, want %q", m.MediaType, manifestAccept)
	}
	configBytes, ok := blobs[m.Config.Digest]
	if !ok {
		t.Fatalf("manifest config digest %s has no corresponding blob", m.Config.Digest)
	}
	if !json.Valid(configBytes) {
		t.Error("manifest config blob is not valid JSON")
	}
	if int64(len(configBytes)) != m.Config.Size {
		t.Errorf("manifest config size = %d, actual blob is %d bytes", m.Config.Size, len(configBytes))
	}
	if len(m.Layers) == 0 {
		t.Fatal("manifest has no layers")
	}

	sawConfig, sawNVRAM, sawDisk := false, false, false
	for _, l := range m.Layers {
		b, ok := blobs[l.Digest]
		if !ok {
			t.Errorf("layer %s (%s) has no corresponding blob", l.Digest, l.MediaType)
			continue
		}
		if int64(len(b)) != l.Size {
			t.Errorf("layer %s size = %d, actual blob is %d bytes", l.Digest, l.Size, len(b))
		}
		switch {
		case l.MediaType == mediaTypeConfig:
			sawConfig = true
		case l.MediaType == mediaTypeNVRAM:
			sawNVRAM = true
		case strings.HasPrefix(l.MediaType, mediaTypeDiskPrefix):
			sawDisk = true
			if _, err := l.uncompressedSize(); err != nil {
				t.Errorf("disk layer %s: %v", l.Digest, err)
			}
		default:
			t.Errorf("unexpected layer media type %s", l.MediaType)
		}
	}
	if !sawConfig || !sawNVRAM || !sawDisk {
		t.Errorf("manifest layers missing a required kind: config=%v nvram=%v disk=%v", sawConfig, sawNVRAM, sawDisk)
	}
}

// TestWriteImageRejectsEmptyNVRAM covers the Bundle.Verify precondition
// WriteImage's own doc comment states: an empty nvram.bin fails Verify even
// though a Windows guest's HCS boot path never reads it, so packing one is a
// mistake WriteImage must catch at pack time, not leave for pull time.
func TestWriteImageRejectsEmptyNVRAM(t *testing.T) {
	cfg := tart.Config{OS: "windows", Arch: "amd64", CPUCount: 1, MemorySize: 1 << 30}
	disk := []byte("disk")
	_, err := WriteImage(t.TempDir(), cfg, bytes.NewReader(disk), int64(len(disk)), nil)
	if err == nil {
		t.Fatal("want an error for empty nvram, got nil")
	}
}

// TestPutDiskLayersRejectsEmptyDisk covers diskSize == 0.
func TestPutDiskLayersRejectsEmptyDisk(t *testing.T) {
	w := &blobWriter{dir: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(w.dir, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := w.putDiskLayers(bytes.NewReader(nil), 0, 1024)
	if err == nil {
		t.Fatal("want an error for an empty disk, got nil")
	}
}

// TestWriteImageRejectsConfigsLoadConfigWouldReject covers the gap a
// code-review found: WriteImage used to happily pack any tart.Config,
// deferring every one of these to a first-pull-time (or worse, first-boot)
// failure instead of catching it deterministically at pack time. Each case
// here is a config LoadConfig (internal/tart/bundle.go) is documented to
// reject.
func TestWriteImageRejectsConfigsLoadConfigWouldReject(t *testing.T) {
	cases := map[string]tart.Config{
		"zero cpuCount":                     {OS: "windows", Arch: "amd64", CPUCount: 0, MemorySize: 1 << 30},
		"zero memorySize":                   {OS: "windows", Arch: "amd64", CPUCount: 1, MemorySize: 0},
		"unsupported guest":                 {OS: "solaris", Arch: "sparc", CPUCount: 1, MemorySize: 1 << 30},
		"darwin missing hardwareModel/ecid": {OS: "darwin", Arch: "arm64", CPUCount: 1, MemorySize: 1 << 30},
	}
	disk := []byte("disk")
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := WriteImage(t.TempDir(), cfg, bytes.NewReader(disk), int64(len(disk)), []byte{0})
			if err == nil {
				t.Fatal("want an error, got nil -- WriteImage packed a config LoadConfig would reject")
			}
		})
	}
}
