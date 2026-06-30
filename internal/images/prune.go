package images

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bojanrajkovic/runny/internal/oci"
	"github.com/bojanrajkovic/runny/internal/versioncore"
)

// PlanItem is one artifact that the prune planner has identified for reclaim.
type PlanItem struct {
	Path   string
	Bytes  int64
	Kind   string // "runner-tarball" | "image-bundle"
	Reason string // "superseded" | "removed pool" | "dead .partial"
	Label  string // human-readable: "<ref> @ sha256:<short-hex>" or filename
}

// tarball is the parsed name + version for the runner-tarball planner.
type tarball struct{ name, version string }

// PlanRunnerCachePrune returns the set of tarball files to delete from
// cacheDir: the same flavour-grouped sort as PruneRunnerCache, minus any
// filename in protect (the live slot's RunnerVersion, which must be kept
// regardless of rank). A nil protect set behaves like an empty one.
// .partial temps are always planned for deletion (dead download).
func PlanRunnerCachePrune(cacheDir string, keep int, protect map[string]bool) ([]PlanItem, error) {
	if keep < 1 {
		return nil, nil
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	groups := map[string][]tarball{}
	var items []PlanItem

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".partial") {
			// Skip if EnsureRunnerTarball holds the semaphore for the
			// destination file — the download is live and the Rename that
			// follows it would fail with ENOENT if we unlink first.
			if _, active := tarballLocks.Load(strings.TrimSuffix(name, ".partial")); active {
				continue
			}
			items = append(items, PlanItem{
				Path:   filepath.Join(cacheDir, name),
				Bytes:  fileSize(filepath.Join(cacheDir, name)),
				Kind:   "runner-tarball",
				Reason: "dead .partial",
				Label:  name,
			})
			continue
		}
		parts := strings.Split(name, "-")
		if len(parts) < 4 {
			continue
		}
		prefix := strings.Join(parts[:4], "-") // actions-runner-<os>-arm64
		v := versioncore.Core(strings.TrimPrefix(name, prefix+"-"))
		if v == "" {
			continue // unparseable: leave alone
		}
		groups[prefix] = append(groups[prefix], tarball{name, v})
	}

	for _, vs := range groups {
		if len(vs) <= keep {
			continue
		}
		slices.SortFunc(vs, func(a, b tarball) int { return versioncore.Compare(b.version, a.version) }) // newest first
		kept := 0
		for _, t := range vs {
			if protect[t.name] {
				continue // live slot's version: never plan for deletion
			}
			if kept < keep {
				kept++
				continue
			}
			items = append(items, PlanItem{
				Path:   filepath.Join(cacheDir, t.name),
				Bytes:  fileSize(filepath.Join(cacheDir, t.name)),
				Kind:   "runner-tarball",
				Reason: "superseded",
				Label:  t.name,
			})
		}
	}
	return items, nil
}

// PlanImageBundlePrune returns the set of bundle directories to delete from
// imagesDir. keepPaths is the set of absolute bundle dir paths to preserve
// (live slot digests + configured-ref resolved digests). protectRefDirNames is
// the set of ref dir basenames to leave entirely intact (resolve failed for
// that ref — fail safe). configuredRefs maps sanitized-ref-dir-basename to the
// original image ref string (for label and reason classification); a ref dir
// not in configuredRefs gets reason "removed pool".
func PlanImageBundlePrune(imagesDir string, keepPaths, protectRefDirNames map[string]bool, configuredRefs map[string]string) ([]PlanItem, error) {
	refEntries, err := os.ReadDir(imagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var items []PlanItem
	var scanErrs []error
	for _, refEntry := range refEntries {
		if !refEntry.IsDir() {
			continue
		}
		refDirName := refEntry.Name()
		if protectRefDirNames[refDirName] {
			continue // resolve failed; leave entire ref dir intact
		}
		originalRef, isConfigured := configuredRefs[refDirName]
		reason := "superseded"
		displayRef := originalRef
		if !isConfigured {
			reason = "removed pool"
			displayRef = refDirName // sanitized form; can't de-sanitize without original
		}

		refDirPath := filepath.Join(imagesDir, refDirName)
		bundleEntries, err := os.ReadDir(refDirPath)
		if err != nil {
			scanErrs = append(scanErrs, err)
			continue
		}
		for _, bundleEntry := range bundleEntries {
			if !bundleEntry.IsDir() {
				continue
			}
			name := bundleEntry.Name()
			bundlePath := filepath.Join(refDirPath, name)

			// OCI pull temp dirs are named "<digest>.partial-<random>". A live
			// pull owns the dir; an orphan from a prior crash is safe to remove.
			if idx := strings.Index(name, ".partial-"); idx >= 0 {
				destDir := filepath.Join(refDirPath, name[:idx])
				if oci.PullInProgress(destDir) {
					continue // live pull; leave it alone
				}
				items = append(items, PlanItem{
					Path:   bundlePath,
					Bytes:  dirSize(bundlePath),
					Kind:   "image-bundle",
					Reason: "dead .partial",
					Label:  displayRef + " @ " + name,
				})
				continue
			}

			if keepPaths[bundlePath] {
				continue
			}
			items = append(items, PlanItem{
				Path:   bundlePath,
				Bytes:  dirSize(bundlePath),
				Kind:   "image-bundle",
				Reason: reason,
				Label:  displayRef + " @ " + digestLabel(name),
			})
		}
	}
	return items, errors.Join(scanErrs...)
}

// ApplyPrune deletes every path in the plan. Best-effort: one failure does not
// stop the rest. Returns the bytes actually freed (only items that succeeded)
// and a joined error for any failures. For image-bundle items it also attempts
// to remove the parent ref dir once all its bundles are gone.
//
// An optional guard func may be passed; items for which it returns false are
// skipped. The caller uses this to re-check live slot state immediately before
// each deletion, collapsing the re-snapshot and apply into a single call.
func ApplyPrune(items []PlanItem, guard ...func(PlanItem) bool) (int64, error) {
	var freed int64
	var errs []error
	refDirs := map[string]bool{}
	var g func(PlanItem) bool
	if len(guard) > 0 {
		g = guard[0]
	}
	for _, item := range items {
		if g != nil && !g(item) {
			continue
		}
		if err := os.RemoveAll(item.Path); err != nil {
			errs = append(errs, err)
		} else {
			freed += item.Bytes
		}
		if item.Kind == "image-bundle" {
			refDirs[filepath.Dir(item.Path)] = true
		}
	}
	for dir := range refDirs {
		_ = os.Remove(dir) // noop if non-empty or already gone
	}
	return freed, errors.Join(errs...)
}

func applyPrune(items []PlanItem) (int64, error) { return ApplyPrune(items) }

// PruneRunnerCache keeps the `keep` newest tarball versions per OS/arch
// flavor in cacheDir and deletes the rest. See the original doc comment in
// images.go. This is now a thin wrapper over PlanRunnerCachePrune + applyPrune.
func PruneRunnerCache(cacheDir string, keep int) error {
	items, err := PlanRunnerCachePrune(cacheDir, keep, nil)
	if err != nil {
		return err
	}
	_, err = applyPrune(items)
	return err
}

// ---- helpers -----------------------------------------------------------------

func fileSize(path string) int64 { return diskBytes(path) }

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		total += diskBytes(p)
		return nil
	})
	return total
}

// digestLabel converts a bundle dir name (sha256-<hex>) to the short display
// form "sha256:<first12hex>".
func digestLabel(dirName string) string {
	digest := strings.Replace(dirName, "-", ":", 1)
	if !strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	hex := digest[7:]
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "sha256:" + hex
}
