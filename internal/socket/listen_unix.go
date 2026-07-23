//go:build !windows

package socket

import (
	"fmt"
	"net"
	"os"
)

// listen binds the control channel for a non-windows daemon: a unix domain
// socket at path, restricted to owner-only (0600) so the socket's own mode is
// the outer authorization tier and the operator ACL inherited from the home
// dir is the second (see docs/security.md). The stale-socket remove handles a
// previous run that exited without unlinking.
func listen(path string) (net.Listener, error) {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("restricting socket perms: %w", err)
	}
	return ln, nil
}
