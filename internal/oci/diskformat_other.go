//go:build !windows

package oci

import "github.com/bojanrajkovic/runny/internal/tart"

// refuseHostDiskFormat accepts every known format off windows: macOS's ASIF
// floor depends on the host version, which internal/vm checks at boot.
func refuseHostDiskFormat(*tart.Config) error { return nil }
