package vhdx

import (
	"encoding/binary"
	"errors"
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

// buildDifferencingFixture builds a minimal in-memory differencing VHDX
// (HasParent=1, and -- unless locatorItem is nil -- a Parent Locator item)
// via buildFixture's shared region/metadata-table construction (reader_test.go).
func buildDifferencingFixture(t *testing.T, locatorItem []byte) []byte {
	t.Helper()
	ex := fixtureExtra{hasParent: true}
	if locatorItem != nil {
		ex.items = []metaItem{{itemParentLocator, locatorItem}}
	}
	return buildFixture(t, 2*1024*1024, 3*1024*1024, []uint64{0}, ex)
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
		"relative_path": `.\gone.vhdx`,                   // stale: does not exist
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
