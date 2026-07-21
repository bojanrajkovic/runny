package vhdx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRead_RealFixture parses testdata/fixed-min.vhdx -- a real fixed VHDX
// produced by CreateVirtualDisk+FullPhysicalAllocation on real Windows
// hardware (see internal/vhdx/CLAUDE.md for how to regenerate it), not a
// synthetic in-code fixture. It's the smallest fixed VHDX the API will
// produce: MaximumSize below 3 MiB fails with ERROR_INVALID_PARAMETER
// regardless of block size, an undocumented floor also recorded there.
func TestRead_RealFixture(t *testing.T) {
	const path = "testdata/fixed-min.vhdx"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	// A git-lfs checkout without `git lfs pull` (or without git-lfs
	// installed) leaves a small pointer-file stub in place of the real
	// binary -- skip with a clear pointer rather than fail confusingly on
	// what looks like corrupt VHDX bytes.
	if strings.HasPrefix(string(raw), "version https://git-lfs") {
		t.Skip("testdata/fixed-min.vhdx is an unresolved git-lfs pointer -- run `git lfs pull` (git-lfs is mise-managed in this repo)")
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()

	info, err := Read(f)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := Info{
		BlockSize:            1024 * 1024,
		LeaveBlockAllocated:  true,
		HasParent:            false,
		VirtualDiskSize:      3 * 1024 * 1024,
		LogicalSectorSize:    512,
		PhysicalSectorSize:   512,
		BATRegionOffset:      3 * 1024 * 1024,
		MetadataRegionOffset: 2 * 1024 * 1024,
		MetadataRegionLength: 1024 * 1024,
	}
	if info != want {
		t.Errorf("Read(%s) = %+v, want %+v", path, info, want)
	}

	entries, err := ReadBAT(f, info)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3 (a 3 MiB disk at a 1 MiB block size)", len(entries))
	}
	for i, e := range entries {
		if e.State != StateFullyPresent {
			t.Errorf("entries[%d].State = %d, want StateFullyPresent", i, e.State)
		}
	}
}

// TestParentLocator_RealFixture resolves testdata/differencing-min.vhdx's
// parent -- a real differencing VHDX produced by New-VHD -Differencing on
// real Windows hardware, parented to testdata/fixed-min.vhdx itself (see
// internal/vhdx/CLAUDE.md for how to regenerate it). Exercises the real
// byte layout Hyper-V writes, not just the synthetic fixtures in
// parentlocator_test.go.
func TestParentLocator_RealFixture(t *testing.T) {
	const path = "testdata/differencing-min.vhdx"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if strings.HasPrefix(string(raw), "version https://git-lfs") {
		t.Skip("testdata/differencing-min.vhdx is an unresolved git-lfs pointer -- run `git lfs pull` (git-lfs is mise-managed in this repo)")
	}

	got, err := ParentLocator(path)
	if err != nil {
		t.Fatalf("ParentLocator(%s): %v", path, err)
	}
	if want := filepath.Join("testdata", "fixed-min.vhdx"); got != want {
		t.Errorf("ParentLocator(%s) = %q, want %q", path, got, want)
	}
}
