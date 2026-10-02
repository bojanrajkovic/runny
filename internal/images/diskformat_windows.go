//go:build windows

package images

import (
	"errors"
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vhdx"
)

// prepareBundleDisk gets a freshly-pulled bundle's disk into disk.vhdx, the
// form the Hyper-V backend's differencing clone needs, then
// removes disk.img — Bundle.Verify accepts either, so this doesn't cost a
// re-pull on the next Ensure. Skipped (past a best-effort disk.img cleanup)
// if disk.vhdx already exists — a retried/resumed pull of an
// already-prepared bundle, including one interrupted between the VHDX
// landing and the disk.img removal below.
//
// A Windows-guest image's disk layer carries VHDX bytes directly (packed
// that way by runnyctl image pack, so a pull of it never round-trips through
// a raw conversion); a Linux-guest image's disk layer is still a raw image
// that needs converting. Both decode to the same disk.img path (internal/oci
// doesn't know or care what's inside the bytes it wrote), so this sniffs the
// content rather than branching on cfg.OS — treating an already-VHDX
// disk.img as raw would hand vhdx.Convert a source it can't ingest anyway
// (SourcePath only accepts a physical disk or another virtual disk, per this
// package's own CLAUDE.md), and skipping the sniff would otherwise silently
// wrap a 30-40GB VHDX in a second one on every Windows image pull.
func prepareBundleDisk(bundle tart.Bundle) error {
	// ASIF is refused before the disk.vhdx-exists check below, not after:
	// internal/oci's pull already refuses a fresh ASIF pull before nvram.bin
	// even downloads, but a bundle pulled by an OLDER build could have been
	// wrapped into disk.vhdx already (before this refusal existed) -- that
	// cached bundle must be refused here too, on every subsequent Ensure,
	// rather than silently passing through to CLONE as if it were a normal
	// VHDX.
	cfg, err := bundle.LoadConfig()
	if err != nil {
		return fmt.Errorf("loading bundle config: %w", err)
	}
	if err := cfg.RefuseASIFOnHyperV(); err != nil {
		return fmt.Errorf("%s: %w", bundle.ConfigPath(), err)
	}

	if _, err := os.Stat(bundle.VHDXPath()); err == nil {
		os.Remove(bundle.DiskPath()) // best-effort: a prior crash may have left this behind
		return nil
	}

	f, err := os.Open(bundle.DiskPath())
	if err != nil {
		return fmt.Errorf("opening %s: %w", bundle.DiskPath(), err)
	}

	// The config label can be wrong (a mislabeled/hand-edited bundle, or one
	// that predates this repo ever writing the label at all) -- sniff the
	// actual bytes too, before handing them to vhdx.Read/vhdx.Convert, both
	// of which would otherwise treat an ASIF disk.img as a bare raw image and
	// wrap it into a VHDX the guest silently can't boot.
	isASIF, err := tart.IsASIFDisk(f)
	if err != nil {
		f.Close()
		return fmt.Errorf("sniffing %s for ASIF magic: %w", bundle.DiskPath(), err)
	}
	if isASIF {
		f.Close()
		return fmt.Errorf("%s is ASIF-framed despite diskFormat %q: %w", bundle.DiskPath(), cfg.DiskFormat, tart.ErrASIFUnsupportedOnHost)
	}

	_, sniffErr := vhdx.Read(f)
	f.Close()
	switch {
	case sniffErr == nil:
		if err := os.Rename(bundle.DiskPath(), bundle.VHDXPath()); err != nil {
			return fmt.Errorf("renaming already-VHDX %s to %s: %w", bundle.DiskPath(), bundle.VHDXPath(), err)
		}
		return nil
	case !errors.Is(sniffErr, vhdx.ErrNotAVHDX):
		return fmt.Errorf("sniffing %s: %w", bundle.DiskPath(), sniffErr)
	}
	if err := vhdx.Convert(bundle.DiskPath(), bundle.VHDXPath()); err != nil {
		return fmt.Errorf("converting %s to VHDX: %w", bundle.DiskPath(), err)
	}
	if err := os.Remove(bundle.DiskPath()); err != nil {
		return fmt.Errorf("removing converted %s: %w", bundle.DiskPath(), err)
	}
	return nil
}
