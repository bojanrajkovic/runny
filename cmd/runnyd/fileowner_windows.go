//go:build windows

package main

import (
	"errors"
	"fmt"
)

// fileOwnerUID has no unix uid to read on windows. Its only caller is the
// launchd-flavored competing-registration probe, which the doctor gates on
// GOOS — this stub keeps the build portable, not the probe.
func fileOwnerUID(path string) (uint32, error) {
	return 0, fmt.Errorf("owner uid of %s: %w", path, errors.ErrUnsupported)
}
