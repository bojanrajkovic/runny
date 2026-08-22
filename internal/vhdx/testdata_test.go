package vhdx

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readFixture decompresses testdata/<name>.gz. Both VHDX fixtures are almost
// entirely zero bytes -- a minimal header/region/metadata skeleton -- so they
// gzip ~900x, and are committed compressed as ordinary git blobs (14 KB for
// the pair, against 11.5 MB raw). That keeps them out of git-lfs, whose
// bandwidth on a public repo bills the repo owner for every clone.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".gz"))
	if err != nil {
		t.Fatalf("opening fixture: %v", err)
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzip %s.gz: %v", name, err)
	}
	defer zr.Close()

	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("decompressing %s.gz: %v", name, err)
	}
	return raw
}

// writeFixture materializes testdata/<name>.gz into dir under its
// uncompressed name, for the APIs that take a path rather than an
// io.ReaderAt. Returns the written path.
func writeFixture(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, readFixture(t, name), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestRead_RealFixture parses testdata/fixed-min.vhdx.gz -- a real fixed VHDX
// produced by CreateVirtualDisk+FullPhysicalAllocation on real Windows
// hardware (see internal/vhdx/CLAUDE.md for how to regenerate it), not a
// synthetic in-code fixture. It's the smallest fixed VHDX the API will
// produce: MaximumSize below 3 MiB fails with ERROR_INVALID_PARAMETER
// regardless of block size, an undocumented floor also recorded there.
func TestRead_RealFixture(t *testing.T) {
	const name = "fixed-min.vhdx"
	r := bytes.NewReader(readFixture(t, name))

	info, err := Read(r)
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
		t.Errorf("Read(%s) = %+v, want %+v", name, info, want)
	}

	entries, err := ReadBAT(r, info)
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

// TestParentLocator_RealFixture resolves testdata/differencing-min.vhdx.gz's
// parent -- a real differencing VHDX produced by New-VHD -Differencing on
// real Windows hardware, parented to fixed-min.vhdx itself (see
// internal/vhdx/CLAUDE.md for how to regenerate it). Exercises the real byte
// layout Hyper-V writes, not just the synthetic fixtures in
// parentlocator_test.go.
//
// Both fixtures are materialized side by side because ParentLocator returns
// the first candidate os.Stat confirms exists: the parent has to be on disk
// next to the child under exactly the name relative_path records
// (".\fixed-min.vhdx"). The other two candidates the fixture carries -- a
// C:\ absolute path and a \\?\Volume{...} GUID path -- only resolve on the
// Windows host that generated it.
func TestParentLocator_RealFixture(t *testing.T) {
	dir := t.TempDir()
	child := writeFixture(t, dir, "differencing-min.vhdx")
	parent := writeFixture(t, dir, "fixed-min.vhdx")

	got, err := ParentLocator(child)
	if err != nil {
		t.Fatalf("ParentLocator(%s): %v", child, err)
	}
	if got != parent {
		t.Errorf("ParentLocator(%s) = %q, want %q", child, got, parent)
	}
}
