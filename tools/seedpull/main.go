// One-off cache seeder: pulls a tart-format image into a runny image-cache
// layout on a box with healthy registry connectivity, for rsync to the
// deployment host. Mirrors images.Ensurer's path scheme exactly.
package main

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/bojanrajkovic/runny/internal/bounded"
	"github.com/bojanrajkovic/runny/internal/home"
	"github.com/bojanrajkovic/runny/internal/oci"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <ref> <home-dir>\n", os.Args[0])
		os.Exit(2)
	}
	ref, err := oci.ParseRef(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse ref:", err)
		os.Exit(1)
	}
	dir := home.Dir(os.Args[2])
	if err := dir.Ensure(); err != nil {
		fmt.Fprintln(os.Stderr, "ensure home:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	client := oci.NewClient()
	rctx, rcancel := context.WithTimeout(ctx, time.Minute)
	digest, err := client.Resolve(rctx, ref)
	rcancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}
	dest := dir.ImageBundleDir(ref.String(), digest)
	fmt.Printf("resolved %s -> %s\npulling to %s\n", ref, digest, dest)

	// Stall-watched like every other download in this repo: a hung
	// registry must fail loudly, not silently block an operator tool.
	stall := bounded.NewStall()
	var total atomic.Int64
	client.Progress = func(n int64) {
		stall.Feed(n)
		total.Add(n)
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				fmt.Printf("pulled %.1f MiB\n", float64(total.Load())/(1<<20))
			}
		}
	}()

	pinned := ref
	pinned.Digest = digest
	wctx, cancel := stall.Watch(ctx, 3*time.Minute)
	defer cancel()
	if _, err := client.PullTo(wctx, pinned, dest); err != nil {
		close(done)
		if cause := context.Cause(wctx); cause != nil && wctx.Err() != nil {
			err = cause
		}
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
	close(done)
	fmt.Printf("done: %.1f MiB -> %s\n", float64(total.Load())/(1<<20), dest)
}
