//go:build windows

package main

import (
	"context"
	"net"

	winio "github.com/Microsoft/go-winio"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// pipeDialAccess is GENERIC_READ|GENERIC_WRITE — the duplex access a gRPC
// client needs on the pipe.
const pipeDialAccess = 0x80000000 | 0x40000000

// dial connects to the daemon's named pipe. The pipe is dialed at
// SECURITY_IDENTIFICATION (winio.PipeImpLevelIdentification): the daemon reads
// the client's SID by impersonating this connection at the handshake, and
// winio's default Anonymous level yields an unreadable anonymous token there
// ("cannot open an anonymous level security token"). Identification lets the
// server read identity and group membership but grants it no rights to act as
// the client — the minimum the operator-revocation read requires.
//
// passthrough:runnyd, not a target grpc must resolve: the real endpoint is the
// context dialer, and passthrough hands the client a single fixed address so no
// resolver ever parses the pipe path (which is not a host:port).
func dial(socketPath string) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:runnyd",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return winio.DialPipeAccessImpLevel(ctx, socketPath, pipeDialAccess, winio.PipeImpLevelIdentification)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
}

// socketPresent steers only connHint's wording. On windows the control channel
// is a named pipe in the kernel's pipe namespace, not a file: os.Stat would
// open it as a client (consuming a pipe instance and tripping a server-side
// handshake), so this reports false and connHint falls back to its
// daemon-not-reachable phrasing.
//
// ponytail: always-false loses the hung-vs-absent distinction on windows; wire
// WaitNamedPipe if that wording ever matters.
func socketPresent(string) bool { return false }
