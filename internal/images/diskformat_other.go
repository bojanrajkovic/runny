//go:build !windows

package images

import "github.com/bojanrajkovic/runny/internal/tart"

// prepareBundleDisk is a no-op off windows: vz boots disk.img directly, no
// conversion needed.
func prepareBundleDisk(tart.Bundle) error { return nil }
