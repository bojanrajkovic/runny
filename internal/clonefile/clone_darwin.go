//go:build darwin

// Package clonefile is a thin wrapper over the APFS clonefile(2) syscall: a
// copy-on-write clone of a single file, near-instant regardless of size because
// only metadata is copied until a block is written. It is the primitive both
// the tart bundle clone (internal/tart) and the per-cycle runner-tarball clone
// (the state machine) build on, so the darwin/non-darwin split lives here once.
package clonefile

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Clone copy-on-write clones src to dst. dst must not already exist —
// clonefile(2) fails (EEXIST) rather than overwriting, which is the guarantee a
// fresh per-cycle mount depends on.
func Clone(src, dst string) error {
	if err := unix.Clonefile(src, dst, 0); err != nil {
		return fmt.Errorf("clonefile %s -> %s: %w", src, dst, err)
	}
	return nil
}
