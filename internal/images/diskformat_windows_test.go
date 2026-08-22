//go:build windows

package images

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

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
