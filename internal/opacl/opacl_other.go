//go:build !darwin && !windows

package opacl

import (
	"errors"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// ErrUnsupported is returned by every opacl operation on a platform with no
// operator ACL mechanism: darwin's extended home-dir ACL and Windows' home
// DACL are the two implementations, and the system daemon they back exists
// only there.
var ErrUnsupported = errors.New("opacl: operator ACL management requires darwin or windows")

func ListIDs(homeDir string) ([]string, error) { return nil, ErrUnsupported }

func Grant(ctx bounded.Context, homeDir, sock, username string) error { return ErrUnsupported }

func Revoke(ctx bounded.Context, homeDir, sock, username string) error { return ErrUnsupported }
