//go:build !windows

package main

import "context"

// runEntry is main's platform seam. Off Windows there is no service
// manager: a no-op passthrough, so run drives its own root context exactly
// as it always has.
func runEntry() error {
	return run(context.Background())
}
