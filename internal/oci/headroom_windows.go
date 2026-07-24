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
//
// This is now a conservative upper bound, not an exact one: a windows-guest
// image whose disk.img is already VHDX-framed (packed via runnyctl image
// pack) never goes through that conversion at all — prepareBundleDisk
// renames it into place instead — so its actual peak usage is closer to
// `total` alone. There's no way to reserve exactly for that case here,
// though: this runs before a single byte is downloaded, so whether the
// incoming image needs converting isn't knowable yet. The cost of staying
// conservative is only ever a pull refused that would have fit, never the
// reverse.
func RequiredHeadroom(total int64) uint64 {
	return uint64(total) + FixedPullHeadroom
}
