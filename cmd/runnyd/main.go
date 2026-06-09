// runnyd is the runner daemon: one crash-only state machine per runner slot,
// serving the runny.v1 control surface over a unix socket.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "runnyd:", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Println("runnyd: skeleton — daemon assembly lands with internal/statemachine")
	return nil
}
