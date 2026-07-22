//go:build windows

package tart

import (
	"fmt"
	"os"
	"time"

	"github.com/bojanrajkovic/runny/internal/clonefile"
	"github.com/bojanrajkovic/runny/internal/vhdx"
)

// CloneVHDX is windows' per-slot ephemeral-disk clone (Clone's Hyper-V
// sibling, issue #308): config.json is small enough to copy plainly, but the
// disk is a VHDX differencing child of src's converted disk.vhdx
// (internal/vhdx.CreateDifferencing) — near-instant regardless of parent
// size, the same reason Clone uses APFS clonefile on darwin instead of a
// full copy. nvram.bin is deliberately NOT copied: HCS's SecureBoot template
// initializes UEFI variables at compute-system creation, and unlike VZ's
// EFIVariableStore there is no schema field for restoring a persisted EFI
// variable blob from a file — the hardware validation behind #308 booted
// clean with no nvram file ever referenced.
func CloneVHDX(src, dst Bundle) (time.Duration, error) {
	if err := src.Verify(); err != nil {
		return 0, err
	}
	if _, err := os.Stat(src.VHDXPath()); err != nil {
		return 0, fmt.Errorf("source bundle has no converted disk.vhdx: %w", err)
	}
	if err := os.MkdirAll(string(dst), 0o755); err != nil {
		return 0, fmt.Errorf("creating clone dir: %w", err)
	}
	start := time.Now()
	if err := clonefile.Clone(src.ConfigPath(), dst.ConfigPath()); err != nil {
		return 0, err
	}
	if err := vhdx.CreateDifferencing(dst.VHDXPath(), src.VHDXPath()); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}
