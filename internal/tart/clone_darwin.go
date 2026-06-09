//go:build darwin

package tart

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// Clone copy-on-write clones a bundle via APFS clonefile: near-instant
// regardless of disk.img size (measured 957µs for a 120GB bundle). The
// destination directory is created; existing contents cause an error.
func Clone(src, dst Bundle) (time.Duration, error) {
	if err := src.Verify(); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(string(dst), 0o755); err != nil {
		return 0, fmt.Errorf("creating clone dir: %w", err)
	}
	start := time.Now()
	for _, f := range BundleFiles {
		if err := unix.Clonefile(filepath.Join(string(src), f), filepath.Join(string(dst), f), 0); err != nil {
			return 0, fmt.Errorf("clonefile %s: %w", f, err)
		}
	}
	return time.Since(start), nil
}
