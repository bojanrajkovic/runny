//go:build !darwin

package tart

import (
	"errors"
	"time"
)

// Clone requires APFS clonefile; VM management is darwin-only.
func Clone(src, dst Bundle) (time.Duration, error) {
	return 0, errors.New("tart.Clone: only supported on darwin")
}
