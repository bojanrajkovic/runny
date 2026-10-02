//go:build windows

package images

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// writeMinimalConfig writes a config.json that satisfies tart.Bundle.LoadConfig
// for a windows guest, with the given diskFormat label ("" is valid too --
// see tart.Bundle.LoadConfig's doc comment).
func writeMinimalConfig(t *testing.T, bundle tart.Bundle, diskFormat string) {
	t.Helper()
	suffix := ""
	if diskFormat != "" {
		suffix = fmt.Sprintf(`,"diskFormat":%q`, diskFormat)
	}
	cfg := fmt.Sprintf(`{"version":1,"os":"windows","arch":"amd64","cpuCount":2,"memorySize":1%s}`, suffix)
	if err := os.WriteFile(bundle.ConfigPath(), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPrepareBundleDiskPassesThroughAnAlreadyVHDXDisk covers a Windows-guest
// image's disk.img decoding straight to VHDX bytes (see runnyctl image
// pack): prepareBundleDisk must rename it to disk.vhdx unchanged rather than
// handing it to vhdx.Convert, which can't ingest a VHDX source at all.
func TestPrepareBundleDiskPassesThroughAnAlreadyVHDXDisk(t *testing.T) {
	// internal/vhdx's fixture is reused rather than duplicated here, and is
	// committed gzip'd (see that package's readFixture) -- decompress it to
	// get the real VHDX bytes prepareBundleDisk has to sniff.
	gz, err := os.Open("../vhdx/testdata/fixed-min.vhdx.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		t.Fatal(err)
	}
	src, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	bundle := tart.Bundle(t.TempDir())
	writeMinimalConfig(t, bundle, tart.DiskFormatRaw)
	if err := os.WriteFile(bundle.DiskPath(), src, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := prepareBundleDisk(bundle); err != nil {
		t.Fatalf("prepareBundleDisk: %v", err)
	}

	if _, err := os.Stat(bundle.DiskPath()); !os.IsNotExist(err) {
		t.Error("disk.img still present after passthrough, want it gone")
	}
	got, err := os.ReadFile(bundle.VHDXPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, src) {
		t.Error("disk.vhdx bytes changed across passthrough, want an exact rename with no conversion")
	}
}

// TestPrepareBundleDiskRefusesASIFLabelWithVHDXPresent covers a bundle an
// older build already converted into disk.vhdx before ASIF rejection
// existed: the label alone must refuse it on every subsequent Ensure,
// before the disk.vhdx-exists early return would otherwise let it pass
// straight through to CLONE.
func TestPrepareBundleDiskRefusesASIFLabelWithVHDXPresent(t *testing.T) {
	bundle := tart.Bundle(t.TempDir())
	writeMinimalConfig(t, bundle, tart.DiskFormatASIF)
	if err := os.WriteFile(bundle.VHDXPath(), []byte("vhdx"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := prepareBundleDisk(bundle)
	if !errors.Is(err, tart.ErrASIFUnsupportedOnHost) {
		t.Fatalf("prepareBundleDisk: want errors.Is tart.ErrASIFUnsupportedOnHost, got %v", err)
	}
}

// TestPrepareBundleDiskRefusesASIFLabelWithDiskImg covers a fresh bundle
// labeled ASIF whose disk.vhdx doesn't exist yet: refused before any
// conversion work, and disk.vhdx must never get created.
func TestPrepareBundleDiskRefusesASIFLabelWithDiskImg(t *testing.T) {
	bundle := tart.Bundle(t.TempDir())
	writeMinimalConfig(t, bundle, tart.DiskFormatASIF)
	if err := os.WriteFile(bundle.DiskPath(), []byte("not a real disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := prepareBundleDisk(bundle)
	if !errors.Is(err, tart.ErrASIFUnsupportedOnHost) {
		t.Fatalf("prepareBundleDisk: want errors.Is tart.ErrASIFUnsupportedOnHost, got %v", err)
	}
	if _, err := os.Stat(bundle.VHDXPath()); !os.IsNotExist(err) {
		t.Error("disk.vhdx created for a refused ASIF-labeled bundle, want it absent")
	}
}

// TestPrepareBundleDiskRefusesASIFMagicDespiteRawLabel covers a mislabeled
// bundle: diskFormat says raw, but the bytes are actually ASIF-framed.
// prepareBundleDisk must sniff and refuse rather than trust the label and
// hand the bytes to vhdx.Convert.
func TestPrepareBundleDiskRefusesASIFMagicDespiteRawLabel(t *testing.T) {
	bundle := tart.Bundle(t.TempDir())
	writeMinimalConfig(t, bundle, tart.DiskFormatRaw)
	disk := append([]byte("shdw"), bytes.Repeat([]byte{0}, 60)...)
	if err := os.WriteFile(bundle.DiskPath(), disk, 0o600); err != nil {
		t.Fatal(err)
	}

	err := prepareBundleDisk(bundle)
	if !errors.Is(err, tart.ErrASIFUnsupportedOnHost) {
		t.Fatalf("prepareBundleDisk: want errors.Is tart.ErrASIFUnsupportedOnHost, got %v", err)
	}
}
