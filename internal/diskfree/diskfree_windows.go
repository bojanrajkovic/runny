//go:build windows

package diskfree

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// AvailableBytes returns the number of bytes available on the volume holding
// path via GetDiskFreeSpaceExW, which reports bytes available to the calling
// user (respecting per-user quotas) — matching the darwin/linux behavior of
// reporting what a write from this process can actually consume.
func AvailableBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, fmt.Errorf("diskfree: %q: %w", path, err)
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, fmt.Errorf("diskfree: GetDiskFreeSpaceEx %q: %w", path, err)
	}
	return free, nil
}
