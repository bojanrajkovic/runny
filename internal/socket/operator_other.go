//go:build !darwin && !windows

package socket

import "os/user"

// refuseGrantTarget has no per-platform refusal here: the grant path is
// unreachable anyway (opacl reports ErrUnsupported before any target could
// be stamped), so mutateOperator fails on the operator-set read long before
// this verdict matters.
func refuseGrantTarget(u *user.User) error { return nil }
