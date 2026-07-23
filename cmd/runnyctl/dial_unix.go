//go:build !windows

package main

import (
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dial connects to the daemon's unix socket. unix: (single colon, opaque — not
// unix://, a scheme+authority form that only happens to work on unix because
// the leading "/" in an absolute unix path completes it to "unix:///abs/path").
func dial(socketPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient("unix:"+socketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// socketPresent reports whether the daemon socket is present on disk. It only
// steers connHint's wording, so a stat race is harmless either way.
func socketPresent(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
