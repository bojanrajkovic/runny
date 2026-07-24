package oci

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

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

// WriteImage packs disk plus cfg and nvram into a tart-format OCI Image
// Layout at dir (created if absent). It is guest-OS-agnostic: disk rides
// through as opaque bytes, so it works whether the caller hands it a raw
// disk image or, for a windows guest, VHDX bytes directly -- WriteImage
// doesn't need to know which (the pull side, internal/images'
// prepareBundleDisk, is what tells them apart).
//
// cfg.DiskFormat is forced to "raw" regardless of what the caller set:
// tart.Bundle.LoadConfig rejects every other value (see bundle.go), and for
// a VHDX-carrying image the field means only "not ASIF" -- the real disk
// framing lives in the bytes, not this label.
//
// nvram must be non-empty: tart.Bundle.Verify rejects an empty nvram.bin,
// even for a windows guest whose HCS boot path never reads NVRAMPath at all.
func WriteImage(dir string, cfg tart.Config, disk io.Reader, nvram []byte) (PackedImage, error) {
	if len(nvram) == 0 {
		return PackedImage{}, errors.New("packing image: nvram must be non-empty (tart.Bundle.Verify requires it)")
	}
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		return PackedImage{}, fmt.Errorf("creating layout dir: %w", err)
	}
	w := &blobWriter{dir: dir}

	cfg.DiskFormat = "raw"
	configBytes, err := json.Marshal(cfg)
	if err != nil {
		return PackedImage{}, fmt.Errorf("marshaling tart config: %w", err)
	}
	configDesc, err := w.put(mediaTypeConfig, configBytes)
	if err != nil {
		return PackedImage{}, err
	}
	diskDescs, err := w.putDiskLayers(disk)
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

// blobWriter content-addresses blobs into dir/blobs/sha256, the OCI Image
// Layout blob store.
type blobWriter struct{ dir string }

func (w *blobWriter) put(mediaType string, b []byte) (descriptor, error) {
	sum := sha256.Sum256(b)
	hexSum := hex.EncodeToString(sum[:])
	if err := os.WriteFile(filepath.Join(w.dir, "blobs", "sha256", hexSum), b, 0o644); err != nil {
		return descriptor{}, fmt.Errorf("writing blob sha256:%s: %w", hexSum, err)
	}
	return descriptor{MediaType: mediaType, Digest: "sha256:" + hexSum, Size: int64(len(b))}, nil
}

// putDiskLayers reads disk in packDiskLayerSize chunks, Apple-LZ4-frames and
// writes each as its own blob, and annotates it with the uncompressed size
// diskLayerSizes (oci.go) needs to place it during a pull -- the exact
// counterpart of what that function validates on the read side.
func (w *blobWriter) putDiskLayers(disk io.Reader) ([]descriptor, error) {
	var descs []descriptor
	buf := make([]byte, packDiskLayerSize)
	for {
		n, err := io.ReadFull(disk, buf)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("reading disk chunk: %w", err)
		}
		if n == 0 {
			break
		}
		var enc bytes.Buffer
		if encErr := appleLZ4Encode(&enc, buf[:n], lz4BlockSize); encErr != nil {
			return nil, fmt.Errorf("compressing disk chunk: %w", encErr)
		}
		desc, putErr := w.put(mediaTypeDiskV2, enc.Bytes())
		if putErr != nil {
			return nil, putErr
		}
		desc.Annotations = map[string]string{annotationUncompressedSize: strconv.Itoa(n)}
		descs = append(descs, desc)
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
	}
	if len(descs) == 0 {
		return nil, errors.New("packing image: disk is empty")
	}
	return descs, nil
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
