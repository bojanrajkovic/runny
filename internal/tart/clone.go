package tart

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/bojanrajkovic/runny/internal/clonefile"
)

// Clone copy-on-write clones a bundle via APFS clonefile: near-instant
// regardless of disk.img size (measured 957µs for a 120GB bundle). The
// destination directory is created; existing bundle files cause an error
// (clonefile(2) refuses to overwrite). The per-file CoW primitive — and the
// darwin-only build constraint that comes with it — lives in internal/clonefile.
func Clone(src, dst Bundle) (time.Duration, error) {
	if err := src.Verify(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(string(dst), 0o755); err != nil {
		return 0, fmt.Errorf("creating clone dir: %w", err)
	}
	start := time.Now()
	for _, f := range BundleFiles {
		if err := clonefile.Clone(filepath.Join(string(src), f), filepath.Join(string(dst), f)); err != nil {
			return 0, err
		}
	}
	return time.Since(start), nil
}
