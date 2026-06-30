package images

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestPlanRunnerCachePruneProtectsLiveVersion: a live RunnerVersion must be
// kept even when it falls outside the newest-N for its flavor.
func TestPlanRunnerCachePruneProtectsLiveVersion(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"actions-runner-osx-arm64-2.321.0.tar.gz", // newest
		"actions-runner-osx-arm64-2.320.0.tar.gz", // 2nd-newest → would be cut at keep=1
		"actions-runner-osx-arm64-2.319.0.tar.gz", // older → DELETE normally
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// 2.319.0 is the live RunnerVersion and must NOT appear in the plan.
	protect := map[string]bool{"actions-runner-osx-arm64-2.319.0.tar.gz": true}
	items, err := PlanRunnerCachePrune(dir, 1, protect)
	if err != nil {
		t.Fatalf("PlanRunnerCachePrune: %v", err)
	}
	for _, item := range items {
		if filepath.Base(item.Path) == "actions-runner-osx-arm64-2.319.0.tar.gz" {
			t.Errorf("live RunnerVersion ended up in the prune plan: %s", item.Path)
		}
	}
	// 2.320.0 should be in the plan (2nd-newest, outside keep=1, not protected)
	var paths []string
	for _, item := range items {
		paths = append(paths, filepath.Base(item.Path))
	}
	if !slices.Contains(paths, "actions-runner-osx-arm64-2.320.0.tar.gz") {
		t.Errorf("expected 2.320.0 in plan (outside keep=1, not protected); plan: %v", paths)
	}
}

// TestPlanImageBundlePruneSuperseded: a digest not in keepPaths (but whose ref
// dir is configured) is planned with reason "superseded".
func TestPlanImageBundlePruneSuperseded(t *testing.T) {
	imagesDir := t.TempDir()
	ref := "ghcr.io_foo_bar"
	oldBundle := filepath.Join(imagesDir, ref, "sha256-aabbccdd")
	newBundle := filepath.Join(imagesDir, ref, "sha256-11223344")
	if err := os.MkdirAll(oldBundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldBundle, "disk.img"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newBundle, 0o700); err != nil {
		t.Fatal(err)
	}

	keepPaths := map[string]bool{newBundle: true}
	configuredRefs := map[string]string{ref: "ghcr.io/foo/bar:latest"}
	items, err := PlanImageBundlePrune(imagesDir, keepPaths, nil, configuredRefs)
	if err != nil {
		t.Fatalf("PlanImageBundlePrune: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d: %v", len(items), items)
	}
	if items[0].Path != oldBundle {
		t.Errorf("item path = %s, want %s", items[0].Path, oldBundle)
	}
	if items[0].Reason != "superseded" {
		t.Errorf("item reason = %s, want superseded", items[0].Reason)
	}
	if want := diskBytes(filepath.Join(oldBundle, "disk.img")); items[0].Bytes != want {
		t.Errorf("item bytes = %d, want %d", items[0].Bytes, want)
	}
}

// TestPlanImageBundlePruneKeptDigestsAreExcluded: both the live digest and the
// configured-ref-resolved digest must not appear in the plan.
func TestPlanImageBundlePruneKeptDigestsAreExcluded(t *testing.T) {
	imagesDir := t.TempDir()
	ref := "ghcr.io_foo_bar"
	keepBundle := filepath.Join(imagesDir, ref, "sha256-11223344")
	if err := os.MkdirAll(keepBundle, 0o700); err != nil {
		t.Fatal(err)
	}

	keepPaths := map[string]bool{keepBundle: true}
	configuredRefs := map[string]string{ref: "ghcr.io/foo/bar:latest"}
	items, err := PlanImageBundlePrune(imagesDir, keepPaths, nil, configuredRefs)
	if err != nil {
		t.Fatalf("PlanImageBundlePrune: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items (only the kept bundle); got %d: %v", len(items), items)
	}
}

// TestPlanImageBundlePruneRemovedPool: a ref dir with no entry in
// configuredRefs gets all its bundles planned with reason "removed pool".
func TestPlanImageBundlePruneRemovedPool(t *testing.T) {
	imagesDir := t.TempDir()
	ref := "ghcr.io_old_image"
	bundle := filepath.Join(imagesDir, ref, "sha256-deadbeef")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "disk.img"), make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}

	// No entry in configuredRefs → "removed pool"
	items, err := PlanImageBundlePrune(imagesDir, nil, nil, nil)
	if err != nil {
		t.Fatalf("PlanImageBundlePrune: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	if items[0].Reason != "removed pool" {
		t.Errorf("item reason = %s, want removed pool", items[0].Reason)
	}
}

// TestPlanImageBundlePruneProtectRefDir: a ref dir in protectRefDirNames is
// left entirely intact (resolve failed → fail safe).
func TestPlanImageBundlePruneProtectRefDir(t *testing.T) {
	imagesDir := t.TempDir()
	ref := "ghcr.io_foo_bar"
	bundle := filepath.Join(imagesDir, ref, "sha256-11223344")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}

	protectRefDirNames := map[string]bool{ref: true}
	items, err := PlanImageBundlePrune(imagesDir, nil, protectRefDirNames, nil)
	if err != nil {
		t.Fatalf("PlanImageBundlePrune: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items (ref dir protected); got %d: %v", len(items), items)
	}
}

// TestPlanRunnerCachePrunePartialIsReaped: an idle .partial temp is planned.
func TestPlanRunnerCachePrunePartialIsReaped(t *testing.T) {
	dir := t.TempDir()
	partial := "actions-runner-osx-arm64-2.321.0.tar.gz.partial"
	if err := os.WriteFile(filepath.Join(dir, partial), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	items, err := PlanRunnerCachePrune(dir, 2, nil)
	if err != nil {
		t.Fatalf("PlanRunnerCachePrune: %v", err)
	}
	if len(items) != 1 || filepath.Base(items[0].Path) != partial {
		t.Errorf("expected plan to contain the .partial file; got %v", items)
	}
	if items[0].Reason != "dead .partial" {
		t.Errorf("item reason = %s, want dead .partial", items[0].Reason)
	}
}

// TestPlanRunnerCachePruneSkipsActivePartial: a .partial file whose base name
// has a live tarballLocks entry is not planned for deletion.
func TestPlanRunnerCachePruneSkipsActivePartial(t *testing.T) {
	dir := t.TempDir()
	base := "actions-runner-osx-arm64-2.321.0.tar.gz"
	partial := base + ".partial"
	if err := os.WriteFile(filepath.Join(dir, partial), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate an active download by inserting a semaphore entry.
	sem := make(chan struct{}, 1)
	tarballLocks.Store(base, sem)
	t.Cleanup(func() { tarballLocks.Delete(base) })

	items, err := PlanRunnerCachePrune(dir, 2, nil)
	if err != nil {
		t.Fatalf("PlanRunnerCachePrune: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("expected 0 items (active download protected); got %v", items)
	}
}

// TestPruneRunnerCacheBackwardCompat: existing cold-start call still works
// (PruneRunnerCache = PlanRunnerCachePrune + applyPrune).
func TestPruneRunnerCacheBackwardCompat(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"actions-runner-osx-arm64-2.321.0.tar.gz",
		"actions-runner-osx-arm64-2.320.0.tar.gz",
		"actions-runner-osx-arm64-2.319.0.tar.gz", // DROP
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PruneRunnerCache(dir, 2); err != nil {
		t.Fatalf("PruneRunnerCache: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("expected 2 files after prune, got %d", len(entries))
	}
}

// TestPlanImageBundlePruneOrphanedPartialIsPlanned: a .partial-* dir with no
// active pullLock is an orphan from a prior crash and must appear in the plan
// with reason "dead .partial".
func TestPlanImageBundlePruneOrphanedPartialIsPlanned(t *testing.T) {
	imagesDir := t.TempDir()
	ref := "ghcr.io_foo_bar"
	refDir := filepath.Join(imagesDir, ref)
	if err := os.MkdirAll(refDir, 0o700); err != nil {
		t.Fatal(err)
	}
	partialDir := filepath.Join(refDir, "sha256-abc123def456.partial-xy9z")
	if err := os.MkdirAll(partialDir, 0o700); err != nil {
		t.Fatal(err)
	}

	configuredRefs := map[string]string{ref: "ghcr.io/foo/bar:latest"}
	items, err := PlanImageBundlePrune(imagesDir, nil, nil, configuredRefs)
	if err != nil {
		t.Fatalf("PlanImageBundlePrune: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d: %v", len(items), items)
	}
	if items[0].Path != partialDir {
		t.Errorf("item path = %s, want %s", items[0].Path, partialDir)
	}
	if items[0].Reason != "dead .partial" {
		t.Errorf("item reason = %q, want \"dead .partial\"", items[0].Reason)
	}
}

// TestProtectActiveTarballs: covers both protection paths — tarballLocks
// (active download) and tarballReserved (post-download, pre-status window).
func TestProtectActiveTarballs(t *testing.T) {
	assetName := "actions-runner-osx-arm64-2.321.0.tar.gz"

	// tarballLocks path: idle semaphore → not protected; held → protected.
	sem := make(chan struct{}, 1)
	tarballLocks.Store(assetName, sem)
	t.Cleanup(func() { tarballLocks.Delete(assetName) })

	protect := map[string]bool{}
	ProtectActiveTarballs(protect)
	if protect[assetName] {
		t.Fatal("idle semaphore: ProtectActiveTarballs should not protect")
	}

	sem <- struct{}{} // simulate active download
	protect = map[string]bool{}
	ProtectActiveTarballs(protect)
	<-sem
	if !protect[assetName] {
		t.Fatalf("held semaphore: ProtectActiveTarballs missed %s", assetName)
	}

	// tarballReserved path: entry present → protected regardless of semaphore.
	tarballReserved.Store(assetName, struct{}{})
	t.Cleanup(func() { tarballReserved.Delete(assetName) })
	protect = map[string]bool{}
	ProtectActiveTarballs(protect)
	if !protect[assetName] {
		t.Fatalf("tarballReserved: ProtectActiveTarballs missed %s", assetName)
	}
}
