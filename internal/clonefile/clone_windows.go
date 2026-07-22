//go:build windows

package clonefile

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Clone copies src to dst. Windows has no ubiquitous copy-on-write primitive
// analogous to APFS clonefile(2) (ReFS block cloning exists but NTFS — the
// common case — doesn't have it), so this is a plain byte-for-byte copy:
// correct everywhere, just not free the way darwin's is. dst must not
// already exist, matching clone_darwin.go's guarantee that a fresh per-cycle
// mount depends on.
func Clone(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("clone %s -> %s: destination already exists", src, dst)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking destination %s: %w", dst, err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("creating %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copying %s to %s: %w", src, dst, err)
	}
	return out.Close()
}
