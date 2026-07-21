package vhdx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// ErrNotDifferencing is returned by ParentLocator when path's File
// Parameters HasParent bit (§2.6.2.1) is unset -- a fixed or dynamic disk,
// which has no parent to resolve.
var ErrNotDifferencing = errors.New("vhdx: not a differencing disk (File Parameters HasParent is unset)")

// locatorTypeVHDX is the only parent-locator type [MS-VHDX] §2.6.2.6.3
// defines. An implementation "MUST validate that it understands" the
// LocatorType field against this -- an unrecognized type is a loud error,
// not a silent skip.
var locatorTypeVHDX = mustGUID("B04AEFB7-D19E-4A81-B789-25B8E9445913")

// parentLocatorKeyOrder is the resolution order [MS-VHDX] §2.6.2.6.3
// mandates: "relative_path, volume_path and then absolute_path." The first
// candidate that resolves to an existing file wins -- ParentLocator's
// answer to the stale-entry-drift question the spec leaves unspecified
// beyond try-in-order-until-one-opens.
var parentLocatorKeyOrder = []string{"relative_path", "volume_path", "absolute_win32_path"}

// ParentLocator resolves the parent VHDX path recorded in path's Parent
// Locator metadata item (§2.6.2.6). It tries relative_path, volume_path,
// then absolute_win32_path in spec order and returns the first candidate
// that os.Stat confirms exists -- real Hyper-V-authored files carry all
// three, and paths drift (a moved parent, a remounted volume) over a disk's
// lifetime, so existence is the only reliable tie-break. Real
// absolute_win32_path values are NOT required to carry the spec's stated
// "\\?\" prefix (verified against actual Hyper-V output); ParentLocator
// does not reject one that lacks it.
//
// Returns ErrNotDifferencing if path is not a differencing disk.
func ParentLocator(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	info, err := Read(f)
	if err != nil {
		return "", err
	}
	if !info.HasParent {
		return "", ErrNotDifferencing
	}
	if info.parentLocatorLength == 0 {
		return "", fmt.Errorf("vhdx: %s has HasParent set but no Parent Locator metadata item", path)
	}

	raw := make([]byte, info.parentLocatorLength)
	if _, err := f.ReadAt(raw, int64(info.MetadataRegionOffset)+int64(info.parentLocatorOffset)); err != nil {
		return "", fmt.Errorf("reading parent locator item: %w", err)
	}

	entries, err := parseParentLocator(raw)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}

	dir := filepath.Dir(path)
	for _, key := range parentLocatorKeyOrder {
		val, ok := entries[key]
		if !ok {
			continue
		}
		candidate := val
		if key == "relative_path" {
			candidate = filepath.Join(dir, filepath.FromSlash(strings.ReplaceAll(val, `\`, "/")))
		}
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("vhdx: %s: no parent locator candidate (%v) resolves to an existing file", path, parentLocatorKeyOrder)
}

// parseParentLocator decodes the Parent Locator Header (20 bytes:
// LocatorType, Reserved, KeyValueCount) and its key-value entry table (12
// bytes each: KeyOffset, ValueOffset, KeyLength, ValueLength -- all
// verified against a real Hyper-V-produced differencing VHDX's raw bytes,
// not just the spec text) into a key->value map.
func parseParentLocator(raw []byte) (map[string]string, error) {
	if len(raw) < 20 {
		return nil, errors.New("parent locator item shorter than its 20-byte header")
	}
	if got := guidAt(raw, 0); got != locatorTypeVHDX {
		return nil, fmt.Errorf("parent locator type %s is not the VHDX locator type this package understands", got)
	}
	kvCount := binary.LittleEndian.Uint16(raw[18:20])

	entries := make(map[string]string, kvCount)
	for i := uint16(0); i < kvCount; i++ {
		off := 20 + uint32(i)*12
		if uint64(off)+12 > uint64(len(raw)) {
			return nil, fmt.Errorf("parent locator entry %d is out of bounds", i)
		}
		keyOff := binary.LittleEndian.Uint32(raw[off : off+4])
		valOff := binary.LittleEndian.Uint32(raw[off+4 : off+8])
		keyLen := binary.LittleEndian.Uint16(raw[off+8 : off+10])
		valLen := binary.LittleEndian.Uint16(raw[off+10 : off+12])
		if uint64(keyOff)+uint64(keyLen) > uint64(len(raw)) || uint64(valOff)+uint64(valLen) > uint64(len(raw)) {
			return nil, fmt.Errorf("parent locator entry %d key/value bytes are out of bounds", i)
		}
		key, err := utf16leToString(raw[keyOff : keyOff+uint32(keyLen)])
		if err != nil {
			return nil, fmt.Errorf("parent locator entry %d key: %w", i, err)
		}
		val, err := utf16leToString(raw[valOff : valOff+uint32(valLen)])
		if err != nil {
			return nil, fmt.Errorf("parent locator entry %d value: %w", i, err)
		}
		entries[key] = val
	}
	return entries, nil
}

// utf16leToString decodes a [MS-VHDX] §2.6.2.6.1 key/value string: UTF-16LE,
// no internal NUL, Length excludes any trailing NUL (so no NUL-terminated
// assumption applies here, unlike elsewhere in this codebase).
func utf16leToString(b []byte) (string, error) {
	if len(b)%2 != 0 {
		return "", errors.New("UTF-16LE string has an odd byte length")
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u16)), nil
}
