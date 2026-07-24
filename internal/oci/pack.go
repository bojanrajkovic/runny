package oci

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sync/errgroup"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// packDiskLayerSize is the uncompressed bytes per disk layer this writer
// chunks at -- 512 MiB, matching tart's own layerLimitBytes
// (cirruslabs/tart Sources/tart/OCI/Layerizer/DiskV2.swift) so packed
// images are comparable to genuine tart output instead of introducing a
// second, arbitrary chunking convention.
const packDiskLayerSize = 512 << 20

// lz4BlockSize is the Apple-LZ4 compression-block granularity WriteImage
// frames each disk layer at. Unrelated to packDiskLayerSize (that's the
// layer boundary; this is the compression unit inside one layer's stream).
// ponytail: arbitrary, not measured -- tune if pack throughput or the
// resulting compression ratio ever matters enough to profile.
const lz4BlockSize = 1 << 20

// mediaTypeOCIConfig is the standard OCI image-config media type. Real tart
// writes a stub of this alongside its own tart.config.v1 layer so the
// manifest's mandatory `config` field resolves to something a generic OCI
// tool recognizes; this writer does the same. runny's own reader (oci.go)
// never looks at it -- only manifest.Layers matters to PullTo.
const mediaTypeOCIConfig = "application/vnd.oci.image.config.v1+json"

// PackedImage is the result of a successful WriteImage: a tart-format OCI
// Image Layout directory (oci-layout + index.json + blobs/sha256/<digest>),
// structurally identical to what `tart push` produces (Cilicon is the
// reference implementation) and pushable to any registry with a generic
// tool, e.g. `oras cp --from-oci-layout Dir tag ref`.
type PackedImage struct {
	Dir    string // the layout root passed to WriteImage
	Digest string // the manifest's own digest, "sha256:..."
}

// WriteImage packs disk (diskSize bytes) plus cfg and nvram into a
// tart-format OCI Image Layout at dir (created if absent). It is
// guest-OS-agnostic: disk rides through as opaque bytes, so it works
// whether the caller hands it a raw disk image or, for a windows guest,
// VHDX bytes directly -- WriteImage doesn't need to know which (the pull
// side, internal/images' prepareBundleDisk, is what tells them apart).
//
// disk is io.ReaderAt, not io.Reader: disk layers compress concurrently
// (see putDiskLayers), each reading its own byte range independently, with
// no shared read-ahead state to reason about. *os.File and bytes.Reader
// (what every caller here actually hands in) both already implement it.
//
// cfg.DiskFormat is forced to "raw" regardless of what the caller set:
// tart.Bundle.LoadConfig rejects every other value (see bundle.go), and for
// a VHDX-carrying image the field means only "not ASIF" -- the real disk
// framing lives in the bytes, not this label.
//
// nvram must be non-empty: tart.Bundle.Verify rejects an empty nvram.bin,
// even for a windows guest whose HCS boot path never reads NVRAMPath at all.
//
// cfg is validated by running it through the real tart.Bundle.LoadConfig
// before anything is written -- not a second, hand-maintained copy of
// LoadConfig's rules that could silently drift from the original, but the
// actual function every pulled bundle is checked against. Without this, a
// bad --cpu-count/--memory-size/--os/--arch, or a darwin config missing
// HardwareModelB64/ECIDB64, packs "successfully" and only fails the first
// time anything tries to boot the image.
func WriteImage(dir string, cfg tart.Config, disk io.ReaderAt, diskSize int64, nvram []byte) (PackedImage, error) {
	if len(nvram) == 0 {
		return PackedImage{}, errors.New("packing image: nvram must be non-empty (tart.Bundle.Verify requires it)")
	}
	cfg.DiskFormat = "raw"
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		return PackedImage{}, fmt.Errorf("marshaling tart config: %w", err)
	}
	if err := validateConfig(configBytes); err != nil {
		return PackedImage{}, fmt.Errorf("packing image: %w", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return PackedImage{}, fmt.Errorf("creating layout dir: %w", err)
	}
	w := &blobWriter{dir: dir}

	configDesc, err := w.put(mediaTypeConfig, configBytes)
	if err != nil {
		return PackedImage{}, err
	}
	diskDescs, err := w.putDiskLayers(disk, diskSize, packDiskLayerSize)
	if err != nil {
		return PackedImage{}, err
	}
	nvramDesc, err := w.put(mediaTypeNVRAM, nvram)
	if err != nil {
		return PackedImage{}, err
	}
	ociConfigBytes, err := json.Marshal(struct {
		Architecture string `json:"architecture"`
		OS           string `json:"os"`
	}{Architecture: cfg.Arch, OS: cfg.OS})
	if err != nil {
		return PackedImage{}, fmt.Errorf("marshaling OCI config stub: %w", err)
	}
	ociConfigDesc, err := w.put(mediaTypeOCIConfig, ociConfigBytes)
	if err != nil {
		return PackedImage{}, err
	}

	manifestBytes, err := json.Marshal(struct {
		SchemaVersion int          `json:"schemaVersion"`
		MediaType     string       `json:"mediaType"`
		Config        descriptor   `json:"config"`
		Layers        []descriptor `json:"layers"`
	}{
		SchemaVersion: 2,
		MediaType:     manifestAccept,
		Config:        ociConfigDesc,
		Layers:        append(append([]descriptor{configDesc}, diskDescs...), nvramDesc),
	})
	if err != nil {
		return PackedImage{}, fmt.Errorf("marshaling manifest: %w", err)
	}
	manifestDesc, err := w.put(manifestAccept, manifestBytes)
	if err != nil {
		return PackedImage{}, err
	}

	if err := writeJSONFile(filepath.Join(dir, "oci-layout"), map[string]string{"imageLayoutVersion": "1.0.0"}); err != nil {
		return PackedImage{}, err
	}
	if err := writeJSONFile(filepath.Join(dir, "index.json"), struct {
		SchemaVersion int          `json:"schemaVersion"`
		Manifests     []descriptor `json:"manifests"`
	}{SchemaVersion: 2, Manifests: []descriptor{manifestDesc}}); err != nil {
		return PackedImage{}, err
	}

	return PackedImage{Dir: dir, Digest: manifestDesc.Digest}, nil
}

// validateConfig round-trips configBytes through tart.Bundle.LoadConfig in a
// scratch directory -- LoadConfig only ever reads ConfigPath(), so a bare
// config.json is a complete enough bundle for its purposes -- to catch
// anything WriteImage's caller got wrong before a single blob is written.
func validateConfig(configBytes []byte) error {
	tmp, err := os.MkdirTemp("", "runny-image-pack-validate-*")
	if err != nil {
		return fmt.Errorf("validating config: %w", err)
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, "config.json"), configBytes, 0o644); err != nil {
		return fmt.Errorf("validating config: %w", err)
	}
	if _, err := tart.Bundle(tmp).LoadConfig(); err != nil {
		return fmt.Errorf("config would fail LoadConfig on pull: %w", err)
	}
	return nil
}

// blobWriter content-addresses blobs into dir/blobs/sha256, the OCI Image
// Layout blob store.
type blobWriter struct{ dir string }

func (w *blobWriter) put(mediaType string, b []byte) (descriptor, error) {
	digest := digestOf(b)
	if err := os.WriteFile(filepath.Join(w.dir, "blobs", "sha256", digest[len("sha256:"):]), b, 0o644); err != nil {
		return descriptor{}, fmt.Errorf("writing blob %s: %w", digest, err)
	}
	return descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(b))}, nil
}

// maxDiskLayerWorkers caps putDiskLayers' concurrency -- matching
// Client.pull's own disk-layer fan-out cap (oci.go), chosen there for
// throughput but doing double duty here as a memory bound: each worker holds
// one uncompressed chunk (up to layerSize) while it compresses, so the cap
// times layerSize is this function's rough peak footprint.
const maxDiskLayerWorkers = 4

// putDiskLayers Apple-LZ4-compresses and writes disk (diskSize bytes) as
// however many layerSize layers it takes, one independent task per layer
// index, dispatched CONCURRENTLY up to maxDiskLayerWorkers at once via
// errgroup.SetLimit: appleLZ4Encode's blocks are self-contained (see
// applelz4.go), so nothing about compressing one layer depends on another,
// and the read side already parallelizes the identical per-layer codec work
// the same way (Client.pull's errgroup).
//
// Reading disk through io.ReaderAt rather than a single sequential
// io.Reader is deliberate, not incidental: an earlier version of this
// function read sequentially into one shared buffer ahead of a
// hand-rolled semaphore, and across several rounds of review kept
// surfacing a new edge of the same problem -- an unbounded worker count,
// then a chunk allocated before its worker slot was confirmed free, then
// the shared read buffer refilled before its slot was confirmed free, then
// early-return-on-read-error orphaning already-dispatched workers. Every
// one of those was a consequence of coordinating a single ordered read
// stream across concurrent consumers by hand. With io.ReaderAt, each task
// reads its own [offset, offset+n) range independently -- no shared
// buffer, no ordering dependency between layers, and no path where a task
// starts before errgroup itself has confirmed a worker slot is free.
// g.Wait() is unconditional (no early return skips it), so a read error in
// one task can never leave siblings dispatched-but-unwaited-for.
//
// Layer order is still load-bearing -- diskLayerSizes (oci.go) derives each
// layer's offset in disk.img from its position in the manifest -- but
// descs is sized up front from diskSize, so each task writes to its own
// pre-existing index with no append, no reallocation, and no mutex: index i
// is touched by exactly one goroutine, and g.Wait() is what establishes
// happens-before for this function's own read of the finished slice.
//
// layerSize is a parameter (WriteImage always passes packDiskLayerSize)
// rather than reading the package constant directly so tests can exercise
// many layers without packing gigabytes of data.
func (w *blobWriter) putDiskLayers(disk io.ReaderAt, diskSize int64, layerSize int) ([]descriptor, error) {
	if diskSize == 0 {
		return nil, errors.New("packing image: disk is empty")
	}
	layerCount := int((diskSize + int64(layerSize) - 1) / int64(layerSize))
	descs := make([]descriptor, layerCount)

	var g errgroup.Group
	g.SetLimit(maxDiskLayerWorkers)
	for i := range layerCount {
		offset := int64(i) * int64(layerSize)
		n := int64(layerSize)
		if remaining := diskSize - offset; remaining < n {
			n = remaining
		}
		g.Go(func() error {
			chunk := make([]byte, n)
			if _, err := disk.ReadAt(chunk, offset); err != nil {
				return fmt.Errorf("reading disk layer at offset %d: %w", offset, err)
			}
			desc, err := w.putDiskLayerStream(chunk)
			if err != nil {
				return err
			}
			descs[i] = desc
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return descs, nil
}

// putDiskLayerStream Apple-LZ4-compresses chunk directly into a temp file
// under blobs/sha256, hashing as it writes, then renames the temp file to
// its content-addressed final name -- unlike blobWriter.put, it never
// buffers the full compressed layer in memory. Disk layers are the one blob
// large enough (up to layerSize, worst case incompressible) that buffering
// the whole compressed output in memory alongside the raw chunk would
// roughly double each concurrent worker's footprint; a Codex review on this
// PR flagged that doubling, multiplied across maxDiskLayerWorkers, as a real
// OOM risk on a multi-GB pack.
func (w *blobWriter) putDiskLayerStream(chunk []byte) (descriptor, error) {
	blobDir := filepath.Join(w.dir, "blobs", "sha256")
	tmp, err := os.CreateTemp(blobDir, ".tmp-*")
	if err != nil {
		return descriptor{}, fmt.Errorf("creating temp blob: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	if err := appleLZ4Encode(io.MultiWriter(tmp, h), chunk, lz4BlockSize); err != nil {
		tmp.Close()
		return descriptor{}, fmt.Errorf("compressing disk chunk: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		return descriptor{}, fmt.Errorf("stating temp blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return descriptor{}, fmt.Errorf("flushing temp blob: %w", err)
	}

	digest := formatDigest(h.Sum(nil))
	finalPath := filepath.Join(blobDir, digest[len("sha256:"):])
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return descriptor{}, fmt.Errorf("renaming temp blob to %s: %w", digest, err)
	}
	renamed = true

	return descriptor{
		MediaType:   mediaTypeDiskV2,
		Digest:      digest,
		Size:        info.Size(),
		Annotations: map[string]string{annotationUncompressedSize: strconv.Itoa(len(chunk))},
	}, nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
