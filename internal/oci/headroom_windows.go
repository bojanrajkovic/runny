//go:build windows

package oci

// RequiredHeadroom is the free-space margin a pull must reserve beyond the
// image's own uncompressed size (total). Windows additionally needs roughly
// a second full-size copy of the image: internal/images' post-pull
// prepareBundleDisk converts disk.img into a fully-allocated disk.vhdx
// before removing disk.img, so both coexist on disk for the length of that
// conversion — a flat margin here would let a pull land successfully and
// then fail deep inside the (potentially hours-later) VHDX conversion step
// instead of being refused up front.
func RequiredHeadroom(total int64) uint64 {
	return uint64(total) + FixedPullHeadroom
}
