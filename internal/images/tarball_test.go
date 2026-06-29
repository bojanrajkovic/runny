package images

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPruneRunnerCache pins the cold-start GC: keep the `keep` newest versions
// PER OS/arch flavor, drop the rest. Grouping by flavor is load-bearing — a host
// with both darwin and linux pools must not lose one flavor's current tarball
// just because the other flavor has more recent versions. Unparseable names and
// .partial temps are left untouched.
func TestPruneRunnerCache(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"actions-runner-osx-arm64-2.321.0.tar.gz",         // osx newest      → keep
		"actions-runner-osx-arm64-2.320.0.tar.gz",         // osx 2nd-newest  → keep
		"actions-runner-osx-arm64-2.319.0.tar.gz",         // osx older       → DROP
		"actions-runner-osx-arm64-2.300.5.tar.gz",         // osx oldest      → DROP
		"actions-runner-linux-arm64-2.100.0.tar.gz",       // linux, lone     → keep (own group)
		"actions-runner-osx-arm64-2.322.0.tar.gz.partial", // temp            → keep
		"actions-runner-osx-arm64-dev.tar.gz",             // unparseable     → keep
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := PruneRunnerCache(dir, 2); err != nil {
		t.Fatalf("PruneRunnerCache: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	slices.Sort(left)
	want := []string{
		"actions-runner-linux-arm64-2.100.0.tar.gz",
		"actions-runner-osx-arm64-2.320.0.tar.gz",
		"actions-runner-osx-arm64-2.321.0.tar.gz",
		"actions-runner-osx-arm64-2.322.0.tar.gz.partial",
		"actions-runner-osx-arm64-dev.tar.gz",
	}
	if !slices.Equal(left, want) {
		t.Errorf("after prune dir holds %v, want %v", left, want)
	}
}

// TestPruneRunnerCacheNoops: a flavor at or under the keep count is untouched,
// and a missing cache dir is not an error (a daemon that never downloaded one).
func TestPruneRunnerCacheNoops(t *testing.T) {
	dir := t.TempDir()
	keep := []string{
		"actions-runner-osx-arm64-2.321.0.tar.gz",
		"actions-runner-osx-arm64-2.320.0.tar.gz",
	}
	for _, f := range keep {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneRunnerCache(dir, 2); err != nil {
		t.Fatalf("PruneRunnerCache: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("prune dropped a tarball at the keep boundary: %d left, want 2", len(entries))
	}

	if err := PruneRunnerCache(filepath.Join(dir, "nonexistent"), 2); err != nil {
		t.Errorf("missing cache dir: got %v, want nil", err)
	}
}
