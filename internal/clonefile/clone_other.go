//go:build !darwin

package clonefile

import "errors"

// Clone requires APFS clonefile(2); VM management is darwin-only.
func Clone(src, dst string) error {
	return errors.New("clonefile: only supported on darwin")
}
