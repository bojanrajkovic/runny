package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

// schemaFile is the committed schema's name, written next to this generator (so
// the golden test can embed it) and referenced from docs/deploy.md by URL.
const schemaFile = "config.schema.json"

func main() {
	write := flag.Bool("write", false, "write "+schemaFile+" next to the generator instead of printing to stdout")
	flag.Parse()

	b, err := Generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configschema:", err)
		os.Exit(1)
	}
	if !*write {
		_, _ = os.Stdout.Write(b)
		return
	}
	// `bazel run` sets BUILD_WORKING_DIRECTORY to the invocation cwd (the repo
	// root); fall back to the cwd so `go run` works too.
	dir := os.Getenv("BUILD_WORKING_DIRECTORY")
	if dir == "" {
		dir, _ = os.Getwd()
	}
	out := filepath.Join(dir, "tools", "configschema", schemaFile)
	if err := os.WriteFile(out, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "configschema:", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "wrote", out)
}
