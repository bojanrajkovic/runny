//go:build darwin

package socket

import (
	"os/user"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// refuseGrantTarget rejects accounts that must never receive an operator
// ACE. On darwin that is exactly root: it already bypasses the socket's
// 0600 mode and every ACL by design, so an ACE for it would be a lie in the
// operator list — an entry the revoke path appears to control but doesn't.
func refuseGrantTarget(u *user.User) error {
	if u.Username == "root" || u.Uid == "0" {
		return status.Error(codes.InvalidArgument, "refusing to grant root")
	}
	return nil
}
