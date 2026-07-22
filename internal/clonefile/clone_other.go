//go:build !darwin && !windows

package clonefile

import "errors"

// Clone requires either APFS clonefile(2) (darwin) or the plain-copy
// fallback (windows, clone_windows.go); this build has neither VM backend
// and exists only for the pure-Go CI leg.
func Clone(src, dst string) error {
	return errors.New("clonefile: only supported on darwin or windows")
}
