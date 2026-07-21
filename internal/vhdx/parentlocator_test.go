package vhdx

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
	"unicode/utf16"

	"github.com/Microsoft/go-winio/pkg/guid"
)

func utf16leBytes(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	b := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

// buildParentLocatorItem encodes a Parent Locator metadata item (header +
// entries + packed key/value string table) from the given key/value pairs,
// in the byte layout verified against a real Hyper-V-produced differencing
// VHDX (see parentlocator.go's parseParentLocator doc comment).
func buildParentLocatorItem(locatorType guid.GUID, kv map[string]string) []byte {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	header := make([]byte, 20)
	raw := locatorType.ToWindowsArray()
	copy(header[0:16], raw[:])
	binary.LittleEndian.PutUint16(header[18:20], uint16(len(keys)))

	entries := make([]byte, len(keys)*12)
	var strings []byte
	stringsBase := uint32(20 + len(entries))
	for i, k := range keys {
		kb := utf16leBytes(k)
		vb := utf16leBytes(kv[k])
		keyOff := stringsBase + uint32(len(strings))
		strings = append(strings, kb...)
		valOff := stringsBase + uint32(len(strings))
		strings = append(strings, vb...)

		off := i * 12
		binary.LittleEndian.PutUint32(entries[off:off+4], keyOff)
		binary.LittleEndian.PutUint32(entries[off+4:off+8], valOff)
		binary.LittleEndian.PutUint16(entries[off+8:off+10], uint16(len(kb)))
		binary.LittleEndian.PutUint16(entries[off+10:off+12], uint16(len(vb)))
	}
	return append(append(header, entries...), strings...)
}

// buildDifferencingFixture builds a minimal in-memory VHDX (region table +
// metadata table with File Parameters HasParent=1, Virtual Disk Size,
// sector sizes, and -- unless locatorItem is nil -- a Parent Locator item)
// around the byte layout reader_test.go's buildFixture uses. Self-contained
// rather than sharing buildFixture: that helper's signature is exercised by
// #304's already-stable BAT-focused tests, and this fixture's needs
// (variable metadata item count, no real BAT contents) diverge enough that
// threading them through a shared signature isn't worth the shared risk.
func buildDifferencingFixture(t *testing.T, locatorItem []byte) []byte {
	t.Helper()

	const (
		metadataRegionOffset = 2 * 1024 * 1024
		metadataRegionLength = 128 * 1024
		batRegionOffset      = 3 * 1024 * 1024
		batRegionLength      = 1024 * 1024
		itemDataBase         = 64 * 1024
	)

	buf := make([]byte, batRegionOffset+batRegionLength)
	copy(buf[0:8], "vhdxfile")

	rt := buf[regionTable1Offset : regionTable1Offset+regionTableSize]
	copy(rt[0:4], "regi")
	binary.LittleEndian.PutUint32(rt[8:12], 2)
	writeRegionEntry := func(idx int, g guid.GUID, fileOffset uint64, length uint32) {
		off := 16 + idx*32
		raw := g.ToWindowsArray()
		copy(rt[off:off+16], raw[:])
		binary.LittleEndian.PutUint64(rt[off+16:off+24], fileOffset)
		binary.LittleEndian.PutUint32(rt[off+24:off+28], length)
	}
	writeRegionEntry(0, regionBAT, batRegionOffset, batRegionLength)
	writeRegionEntry(1, regionMetadata, metadataRegionOffset, metadataRegionLength)
	binary.LittleEndian.PutUint32(rt[4:8], crc32.Checksum(rt, castagnoliTable))

	mt := buf[metadataRegionOffset : metadataRegionOffset+metadataRegionLength]
	copy(mt[0:8], "metadata")

	items := []struct {
		g      guid.GUID
		offset uint32
		length uint32
	}{
		{itemFileParameters, itemDataBase, 8},
		{itemVirtualDiskSize, itemDataBase + 8, 8},
		{itemLogicalSectorSize, itemDataBase + 16, 4},
		{itemPhysicalSectorSize, itemDataBase + 20, 4},
	}
	locatorOffset := itemDataBase + 24
	if locatorItem != nil {
		items = append(items, struct {
			g      guid.GUID
			offset uint32
			length uint32
		}{itemParentLocator, uint32(locatorOffset), uint32(len(locatorItem))})
	}
	binary.LittleEndian.PutUint16(mt[10:12], uint16(len(items)))
	for i, it := range items {
		off := uint32(32 + i*32)
		raw := it.g.ToWindowsArray()
		copy(mt[off:off+16], raw[:])
		binary.LittleEndian.PutUint32(mt[off+16:off+20], it.offset)
		binary.LittleEndian.PutUint32(mt[off+20:off+24], it.length)
	}

	binary.LittleEndian.PutUint32(mt[itemDataBase:itemDataBase+4], 2*1024*1024) // BlockSize
	binary.LittleEndian.PutUint32(mt[itemDataBase+4:itemDataBase+8], 0x2)       // HasParent=1
	binary.LittleEndian.PutUint64(mt[itemDataBase+8:itemDataBase+16], 3*1024*1024)
	binary.LittleEndian.PutUint32(mt[itemDataBase+16:itemDataBase+20], 512)
	binary.LittleEndian.PutUint32(mt[itemDataBase+20:itemDataBase+24], 512)
	if locatorItem != nil {
		copy(mt[locatorOffset:locatorOffset+len(locatorItem)], locatorItem)
	}

	return buf
}

func writeFixtureFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func TestParentLocator_NotDifferencing(t *testing.T) {
	dir := t.TempDir()
	path := writeFixtureFile(t, dir, "fixed.vhdx", buildFixture(t, 1024*1024, 3*1024*1024, []uint64{0, 1, 2}))

	if _, err := ParentLocator(path); !errors.Is(err, ErrNotDifferencing) {
		t.Errorf("ParentLocator error = %v, want ErrNotDifferencing", err)
	}
}

func TestParentLocator_RelativePath(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "parent.vhdx", []byte("parent bytes"))
	item := buildParentLocatorItem(locatorTypeVHDX, map[string]string{
		"parent_linkage": "{00000000-0000-0000-0000-000000000000}",
		"relative_path":  `.\parent.vhdx`,
	})
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, item))

	got, err := ParentLocator(child)
	if err != nil {
		t.Fatalf("ParentLocator: %v", err)
	}
	if want := filepath.Join(dir, "parent.vhdx"); got != want {
		t.Errorf("ParentLocator = %q, want %q", got, want)
	}
}

func TestParentLocator_SpecOrder_RelativeBeforeVolume(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "chosen.vhdx", []byte("chosen"))
	writeFixtureFile(t, dir, "other.vhdx", []byte("other"))
	item := buildParentLocatorItem(locatorTypeVHDX, map[string]string{
		"relative_path": `.\chosen.vhdx`,
		"volume_path":   filepath.Join(dir, "other.vhdx"), // real path, but spec order must not prefer it
	})
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, item))

	got, err := ParentLocator(child)
	if err != nil {
		t.Fatalf("ParentLocator: %v", err)
	}
	if want := filepath.Join(dir, "chosen.vhdx"); got != want {
		t.Errorf("ParentLocator = %q, want %q (relative_path must win over volume_path)", got, want)
	}
}

func TestParentLocator_FallsThroughStaleCandidate(t *testing.T) {
	dir := t.TempDir()
	writeFixtureFile(t, dir, "real.vhdx", []byte("real"))
	item := buildParentLocatorItem(locatorTypeVHDX, map[string]string{
		"relative_path": `.\gone.vhdx`,                    // stale: does not exist
		"volume_path":   filepath.Join(dir, "real.vhdx"), // exists
	})
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, item))

	got, err := ParentLocator(child)
	if err != nil {
		t.Fatalf("ParentLocator: %v", err)
	}
	if want := filepath.Join(dir, "real.vhdx"); got != want {
		t.Errorf("ParentLocator = %q, want %q (must fall through a stale relative_path)", got, want)
	}
}

func TestParentLocator_NoCandidateResolves(t *testing.T) {
	dir := t.TempDir()
	item := buildParentLocatorItem(locatorTypeVHDX, map[string]string{
		"relative_path": `.\gone.vhdx`,
	})
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, item))

	if _, err := ParentLocator(child); err == nil {
		t.Error("ParentLocator = nil error, want an error (no candidate resolves)")
	}
}

func TestParentLocator_UnknownLocatorType(t *testing.T) {
	dir := t.TempDir()
	wrongType := mustGUID("11111111-1111-1111-1111-111111111111")
	item := buildParentLocatorItem(wrongType, map[string]string{"relative_path": `.\parent.vhdx`})
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, item))

	if _, err := ParentLocator(child); err == nil {
		t.Error("ParentLocator = nil error, want an error (unrecognized locator type)")
	}
}

func TestParentLocator_NoParentLocatorItem(t *testing.T) {
	dir := t.TempDir()
	// HasParent=1 but locatorItem=nil -- malformed: the spec says a
	// differencing disk MUST carry one.
	child := writeFixtureFile(t, dir, "child.vhdx", buildDifferencingFixture(t, nil))

	if _, err := ParentLocator(child); err == nil {
		t.Error("ParentLocator = nil error, want an error (HasParent set, no Parent Locator item)")
	}
}
