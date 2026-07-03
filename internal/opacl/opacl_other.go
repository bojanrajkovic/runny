//go:build !darwin

package opacl

import (
	"errors"

	"github.com/bojanrajkovic/runny/internal/bounded"
)

// ErrUnsupported is returned by every opacl operation on a non-darwin host:
// the ACL mechanism (and the system daemon it backs) is darwin-only.
var ErrUnsupported = errors.New("opacl: operator ACL management requires darwin")

func ListUIDs(homeDir string) ([]uint32, error) { return nil, ErrUnsupported }

func List(homeDir string) ([]Operator, error) { return nil, ErrUnsupported }

func Grant(ctx bounded.Context, homeDir, sock, username string) error { return ErrUnsupported }

func Revoke(ctx bounded.Context, homeDir, sock, username string) error { return ErrUnsupported }
