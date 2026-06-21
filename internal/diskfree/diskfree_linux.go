//go:build linux

package diskfree

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// AvailableBytes returns the number of bytes available on the volume holding
// path via statfs(2).
func AvailableBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, fmt.Errorf("statfs: unexpected block size %d for %q", st.Bsize, path)
	}
	return st.Bavail * uint64(st.Bsize), nil
}
