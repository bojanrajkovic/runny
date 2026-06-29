//go:build darwin

package clonefile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestClone copy-on-write clones a single file and proves the clone is a fully
// independent copy (APFS CoW: the clone shares blocks until written, but reads
// as its own file and a later write to the source must not bleed through).
func TestClone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	want := []byte("runner-tarball-bytes")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Clone(src, dst); err != nil {
		t.Fatalf("Clone: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading clone: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("clone contents = %q, want %q", got, want)
	}

	// Overwriting the source must not change the clone (independent inode).
	if err := os.WriteFile(src, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(want) {
		t.Fatalf("clone changed after source write: %q", got)
	}
}

// TestCloneRefusesExistingDst: clonefile(2) fails if the destination exists,
// which is the property the per-slot mount relies on (a fresh clone per cycle,
// never an overwrite of a live mount).
func TestCloneRefusesExistingDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Clone(src, dst); err == nil {
		t.Fatal("Clone overwrote an existing destination; want error")
	}
}
