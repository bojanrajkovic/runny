//go:build windows

package images

import (
	"bytes"
	"os"
	"testing"

	"github.com/bojanrajkovic/runny/internal/tart"
)

// TestPrepareBundleDiskPassesThroughAnAlreadyVHDXDisk covers a Windows-guest
// image's disk.img decoding straight to VHDX bytes (see runnyctl image
// pack): prepareBundleDisk must rename it to disk.vhdx unchanged rather than
// handing it to vhdx.Convert, which can't ingest a VHDX source at all.
func TestPrepareBundleDiskPassesThroughAnAlreadyVHDXDisk(t *testing.T) {
	src, err := os.ReadFile("../vhdx/testdata/fixed-min.vhdx")
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
