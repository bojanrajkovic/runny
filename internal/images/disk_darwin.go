//go:build darwin

package images

import "syscall"

// diskBytes returns the physical bytes allocated for the file at path.
// On APFS, COW-cloned files share blocks with the original image; Blocks
// reflects only the unique 512-byte blocks, so this gives an accurate
// estimate of how much df will move when the file is removed.
func diskBytes(path string) int64 {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0
	}
	return st.Blocks * 512 // Blocks is in 512-byte units on Darwin
}
