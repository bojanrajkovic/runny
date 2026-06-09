// runnyctl is the control CLI for runnyd, speaking runny.v1 over the daemon's
// unix socket.
package main

import (
	"fmt"
	"os"

	runnyv1 "github.com/bojanrajkovic/runny/proto/runny/v1"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnyctl:", err)
		os.Exit(1)
	}
}

func run() error {
	// Skeleton round-trip through the in-graph generated contract.
	s := runnyv1.SlotState_SLOT_STATE_BACKOFF
	fmt.Printf("runnyctl: skeleton — contract wired (%s)\n", s)
	return nil
}
