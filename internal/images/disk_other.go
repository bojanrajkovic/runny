//go:build !darwin

package images

import "os"

// diskBytes returns the apparent size of the file at path. Off Darwin this is
// a best-effort fallback; the images package only runs meaningfully on Darwin.
func diskBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}
