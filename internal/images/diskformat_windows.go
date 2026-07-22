//go:build windows

package images

import (
	"fmt"
	"os"

	"github.com/bojanrajkovic/runny/internal/tart"
	"github.com/bojanrajkovic/runny/internal/vhdx"
)

// prepareBundleDisk converts a freshly-pulled bundle's raw disk.img into the
// fixed disk.vhdx the Hyper-V backend's differencing clone needs (issue
// #308), then removes disk.img — Bundle.Verify accepts either, so this
// doesn't cost a re-pull on the next Ensure. Skipped (past a best-effort
// disk.img cleanup) if disk.vhdx already exists — a retried/resumed pull of
// an already-converted bundle, including one interrupted between Convert
// succeeding and the disk.img removal below.
func prepareBundleDisk(bundle tart.Bundle) error {
	if _, err := os.Stat(bundle.VHDXPath()); err == nil {
		os.Remove(bundle.DiskPath()) // best-effort: a prior crash may have left this behind
		return nil
	}
	if err := vhdx.Convert(bundle.DiskPath(), bundle.VHDXPath()); err != nil {
		return fmt.Errorf("converting %s to VHDX: %w", bundle.DiskPath(), err)
	}
	if err := os.Remove(bundle.DiskPath()); err != nil {
		return fmt.Errorf("removing converted %s: %w", bundle.DiskPath(), err)
	}
	return nil
}
