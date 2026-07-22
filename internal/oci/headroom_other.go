//go:build !windows

package oci

// RequiredHeadroom is the free-space margin a pull must reserve beyond the
// image's own uncompressed size (total). Off windows nothing further
// processes the pulled disk in place, so the flat margin is the whole
// requirement (see headroom_windows.go for why windows needs more).
func RequiredHeadroom(total int64) uint64 {
	return FixedPullHeadroom
}
