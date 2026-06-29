package images

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSupersededTarballs pins the version-aware supersede rule: only STRICTLY
// OLDER same-flavor tarballs are dropped. The regression it guards is a slot
// staging version N deleting a concurrent slot's freshly-downloaded version
// N+1 (same prefix, different per-assetName lock) — which would leave that
// slot's guest with nothing to stage.
func TestSupersededTarballs(t *testing.T) {
	dir := t.TempDir()
	const self = "actions-runner-osx-arm64-2.320.0.tar.gz"
	files := []string{
		self,
		"actions-runner-osx-arm64-2.319.0.tar.gz",         // older  → drop
		"actions-runner-osx-arm64-2.300.5.tar.gz",         // older  → drop
		"actions-runner-osx-arm64-2.321.0.tar.gz",         // newer  → KEEP (the bug)
		"actions-runner-osx-arm64-2.320.0.tar.gz.partial", // temp   → keep
		"actions-runner-linux-arm64-2.100.0.tar.gz",       // flavor → keep
		"actions-runner-osx-arm64-dev.tar.gz",             // unparseable → keep
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	got := supersededTarballs(dir, self)
	want := []string{
		filepath.Join(dir, "actions-runner-osx-arm64-2.300.5.tar.gz"),
		filepath.Join(dir, "actions-runner-osx-arm64-2.319.0.tar.gz"),
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("supersededTarballs dropped %v, want exactly %v", got, want)
	}
}

// TestSupersededTarballsGuards covers the shape guards: a renamed asset whose
// name has fewer than four dash segments, or no parseable version, drops
// nothing rather than panicking or guessing.
func TestSupersededTarballsGuards(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"weird.tar.gz", "actions-runner-osx-arm64-2.319.0.tar.gz"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := supersededTarballs(dir, "weird.tar.gz"); got != nil {
		t.Errorf("malformed assetName: got %v, want nil", got)
	}
	if got := supersededTarballs(dir, "actions-runner-osx-arm64-dev.tar.gz"); got != nil {
		t.Errorf("unparseable version: got %v, want nil", got)
	}
}
