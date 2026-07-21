// Package vhdx converts a raw disk image into a FIXED (fully-allocated)
// VHDX via Windows' native virtual-disk API (CreateVirtualDisk with
// CreateVirtualDiskFlagFullPhysicalAllocation) rather than a hand-rolled
// [MS-VHDX] writer: the native API produces spec-perfect structure by
// construction, eliminating an entire class of binary-format bugs a
// from-scratch writer would risk. This file (the structural reader) is
// pure Go with no Windows dependency: it exists to verify Convert's output
// shape in cross-platform-buildable tests, and is written against the same
// [MS-VHDX] spec sections (Protocol Revision 8.0, 2024-04-23) a
// differencing-disk parent-locator reader would need, so that work can
// reuse it rather than re-deriving the region/metadata table walk.
package vhdx

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/Microsoft/go-winio/pkg/guid"
)

var castagnoliTable = crc32.MakeTable(crc32.Castagnoli)

// mustGUID parses a well-known spec GUID string at package-init time; the
// six calls below are compile-time-known literals, never user input.
func mustGUID(s string) guid.GUID {
	g, err := guid.FromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

// Known region and metadata-item GUIDs. [MS-VHDX] §2.2.3.2 (regions),
// §2.6.2 (metadata items).
var (
	regionBAT      = mustGUID("2DC27766-F623-4200-9D64-115E9BFD4A08")
	regionMetadata = mustGUID("8B7CA206-4790-4B9A-B8FE-575F050F886E")

	itemFileParameters     = mustGUID("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	itemVirtualDiskSize    = mustGUID("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	itemVirtualDiskID      = mustGUID("BECA12AB-B2E6-4523-93EF-C309E000C746")
	itemLogicalSectorSize  = mustGUID("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	itemPhysicalSectorSize = mustGUID("CDA348C7-445D-4471-9CC9-E9885251C556")
)

// guidAt reads the 16-byte Windows-mixed-endian GUID at b[off:off+16].
func guidAt(b []byte, off uint32) guid.GUID {
	var raw [16]byte
	copy(raw[:], b[off:off+16])
	return guid.FromWindowsArray(raw)
}

// File layout offsets fixed by [MS-VHDX] §2.2: the header section occupies
// the first 1 MiB, with the two region-table copies at 192 KiB and 256 KiB
// (redundant so a corrupted primary copy can fall back to the secondary).
const (
	regionTable1Offset = 192 * 1024
	regionTable2Offset = 256 * 1024
	regionTableSize    = 64 * 1024
)

// regionTableChecksumValid reports whether rt's stored CRC32C checksum
// (§2.2.3.1, offset 4, computed over the whole 64 KiB structure with the
// checksum field itself zeroed) matches its actual contents.
func regionTableChecksumValid(rt []byte) bool {
	want := binary.LittleEndian.Uint32(rt[4:8])
	buf := make([]byte, len(rt))
	copy(buf, rt)
	binary.LittleEndian.PutUint32(buf[4:8], 0)
	return crc32.Checksum(buf, castagnoliTable) == want
}

// readRegionTable reads and validates one region-table copy (signature +
// checksum), returning ok=false rather than an error so the caller can fall
// back to the redundant copy without treating a bad primary as fatal.
func readRegionTable(r io.ReaderAt, offset int64) (rt []byte, ok bool, err error) {
	rt = make([]byte, regionTableSize)
	if _, err := r.ReadAt(rt, offset); err != nil {
		return nil, false, fmt.Errorf("reading region table @%d: %w", offset, err)
	}
	if string(rt[0:4]) != "regi" || !regionTableChecksumValid(rt) {
		return rt, false, nil
	}
	return rt, true, nil
}

// ErrNotAVHDX is returned when the file identifier signature doesn't match
// [MS-VHDX] §2.2.1.
var ErrNotAVHDX = errors.New("vhdx: not a VHDX file (bad file identifier signature)")

// Info summarizes the structural facts [MS-VHDX] requires every VHDX (fixed,
// dynamic, or differencing — §2.1: the layout is identical) to carry, plus
// the region locations needed to walk the BAT.
type Info struct {
	BlockSize            uint32
	LeaveBlockAllocated  bool
	HasParent            bool
	VirtualDiskSize      uint64
	LogicalSectorSize    uint32
	PhysicalSectorSize   uint32
	BATRegionOffset      uint64
	MetadataRegionOffset uint64
	MetadataRegionLength uint32
}

// Read parses the region table and metadata table of the VHDX in r,
// returning the structural facts Info captures. It does not read the BAT or
// any payload bytes — see ReadBAT.
func Read(r io.ReaderAt) (Info, error) {
	var info Info

	fileID := make([]byte, 8)
	if _, err := r.ReadAt(fileID, 0); err != nil {
		return info, fmt.Errorf("reading file identifier: %w", err)
	}
	if string(fileID) != "vhdxfile" {
		return info, ErrNotAVHDX
	}

	rt, ok, err := readRegionTable(r, regionTable1Offset)
	if err != nil {
		return info, err
	}
	if !ok {
		// Primary copy has a bad signature or failed its checksum -- fall
		// back to the redundant secondary copy, §2.2.3, rather than parse
		// a corrupted table and silently return wrong region offsets.
		rt, ok, err = readRegionTable(r, regionTable2Offset)
		if err != nil {
			return info, err
		}
		if !ok {
			return info, errors.New("vhdx: both region table copies are invalid (bad signature or checksum)")
		}
	}
	entryCount := binary.LittleEndian.Uint32(rt[8:12])
	if entryCount > 2047 {
		return info, fmt.Errorf("vhdx: region table entryCount %d exceeds spec max 2047", entryCount)
	}

	var haveBAT, haveMeta bool
	for i := uint32(0); i < entryCount; i++ {
		off := 16 + i*32
		g := guidAt(rt, off)
		fileOffset := binary.LittleEndian.Uint64(rt[off+16 : off+24])
		length := binary.LittleEndian.Uint32(rt[off+24 : off+28])
		switch g {
		case regionBAT:
			info.BATRegionOffset = fileOffset
			haveBAT = true
		case regionMetadata:
			info.MetadataRegionOffset = fileOffset
			info.MetadataRegionLength = length
			haveMeta = true
		}
	}
	if !haveBAT {
		return info, errors.New("vhdx: region table has no BAT region")
	}
	if !haveMeta {
		return info, errors.New("vhdx: region table has no metadata region")
	}

	if err := readMetadata(r, &info); err != nil {
		return info, err
	}
	return info, nil
}

func readMetadata(r io.ReaderAt, info *Info) error {
	hdr := make([]byte, 32)
	if _, err := r.ReadAt(hdr, int64(info.MetadataRegionOffset)); err != nil {
		return fmt.Errorf("reading metadata table header: %w", err)
	}
	if string(hdr[0:8]) != "metadata" {
		return errors.New("vhdx: bad metadata table signature")
	}
	entryCount := binary.LittleEndian.Uint16(hdr[10:12])
	if entryCount > 2047 {
		return fmt.Errorf("vhdx: metadata entryCount %d exceeds spec max 2047", entryCount)
	}

	entries := make([]byte, uint32(entryCount)*32)
	if len(entries) > 0 {
		if _, err := r.ReadAt(entries, int64(info.MetadataRegionOffset)+32); err != nil {
			return fmt.Errorf("reading metadata table entries: %w", err)
		}
	}

	var sawFileParameters, sawVirtualDiskSize bool
	for i := uint16(0); i < entryCount; i++ {
		off := uint32(i) * 32
		g := guidAt(entries, off)
		itemOffset := binary.LittleEndian.Uint32(entries[off+16 : off+20])
		itemLength := binary.LittleEndian.Uint32(entries[off+20 : off+24])
		if itemLength == 0 {
			continue
		}
		// itemOffset/itemLength come straight from file bytes; bound them
		// against the region's own declared size before allocating, so a
		// corrupt or crafted length can't force an unbounded allocation.
		if uint64(itemOffset)+uint64(itemLength) > uint64(info.MetadataRegionLength) {
			return fmt.Errorf("vhdx: metadata item %d [offset=%d length=%d] exceeds metadata region length %d",
				i, itemOffset, itemLength, info.MetadataRegionLength)
		}
		data := make([]byte, itemLength)
		if _, err := r.ReadAt(data, int64(info.MetadataRegionOffset)+int64(itemOffset)); err != nil {
			return fmt.Errorf("reading metadata item %d: %w", i, err)
		}
		switch g {
		case itemFileParameters:
			if len(data) < 8 {
				return errors.New("vhdx: File Parameters item too short")
			}
			blockSize := binary.LittleEndian.Uint32(data[0:4])
			if blockSize == 0 {
				// A zero BlockSize would divide-by-zero in ChunkRatio (§2.5)
				// downstream -- reject it here, at the one place untrusted
				// bytes become a BlockSize, rather than in every caller.
				return errors.New("vhdx: File Parameters BlockSize is 0")
			}
			info.BlockSize = blockSize
			flags := binary.LittleEndian.Uint32(data[4:8])
			info.LeaveBlockAllocated = flags&0x1 != 0
			info.HasParent = flags&0x2 != 0
			sawFileParameters = true
		case itemVirtualDiskSize:
			if len(data) < 8 {
				return errors.New("vhdx: Virtual Disk Size item too short")
			}
			info.VirtualDiskSize = binary.LittleEndian.Uint64(data[0:8])
			sawVirtualDiskSize = true
		case itemVirtualDiskID:
			// Present and well-formed per §2.6.2.3; not surfaced in Info —
			// no current caller needs the disk-identity GUID itself.
		case itemLogicalSectorSize:
			if len(data) < 4 {
				return errors.New("vhdx: Logical Sector Size item too short")
			}
			info.LogicalSectorSize = binary.LittleEndian.Uint32(data[0:4])
		case itemPhysicalSectorSize:
			if len(data) < 4 {
				return errors.New("vhdx: Physical Sector Size item too short")
			}
			info.PhysicalSectorSize = binary.LittleEndian.Uint32(data[0:4])
		}
	}
	if !sawFileParameters {
		return errors.New("vhdx: metadata table missing required File Parameters item")
	}
	if !sawVirtualDiskSize {
		return errors.New("vhdx: metadata table missing required Virtual Disk Size item")
	}
	return nil
}

// BATEntryState is a BAT entry's low 3 bits. [MS-VHDX] §2.5.1.1 (payload
// blocks), §2.5.1.2 (sector-bitmap blocks) — the two enums share encodings
// for the states this package produces/checks (6 fully-present/present is
// the only state Convert ever writes; state 0, not-present, appears in the
// interleaved sector-bitmap slots but has no exported name here since no
// caller compares against it yet).
type BATEntryState uint8

const (
	StateFullyPresent BATEntryState = 6 // PAYLOAD_BLOCK_FULLY_PRESENT / SB_BLOCK_PRESENT
)

// BATEntry is one 8-byte BAT entry, §2.5.1: 3-bit state, 17 reserved bits,
// 44-bit file offset in 1 MiB units.
type BATEntry struct {
	State        BATEntryState
	FileOffsetMB uint64
}

// ChunkRatio is the number of payload-block BAT entries per interleaved
// sector-bitmap slot: Floor(2^23 * LogicalSectorSize / BlockSize) (§2.5).
// Returns 0 for blockSize == 0 (an invalid disk) rather than dividing by
// zero — callers already treat a 0 result as an error (ReadBAT).
func ChunkRatio(logicalSectorSize, blockSize uint32) uint64 {
	if blockSize == 0 {
		return 0
	}
	return (uint64(1) << 23) * uint64(logicalSectorSize) / uint64(blockSize)
}

// PayloadBlockCount is Ceil(VirtualDiskSize / BlockSize) (§2.5), computed
// as a truncating divide plus a conditional +1 rather than the more
// obvious (virtualDiskSize+blockSize-1)/blockSize, which overflows uint64
// silently (wrapping to a small, wrong answer) for a virtualDiskSize near
// math.MaxUint64. Returns 0 for blockSize == 0.
func PayloadBlockCount(virtualDiskSize uint64, blockSize uint32) uint64 {
	bs := uint64(blockSize)
	if bs == 0 {
		return 0
	}
	n := virtualDiskSize / bs
	if virtualDiskSize%bs != 0 {
		n++
	}
	return n
}

// ReadBAT reads every BAT entry for the disk info describes — payload
// blocks and their interleaved sector-bitmap slots (§2.5, Figure 6) — in
// file order. Entry index i's meaning (payload block N vs. the
// sector-bitmap slot following chunk N) is recoverable from ChunkRatio and
// PayloadBlockCount; ReadBAT itself just returns the raw entries.
func ReadBAT(r io.ReaderAt, info Info) ([]BATEntry, error) {
	chunkRatio := ChunkRatio(info.LogicalSectorSize, info.BlockSize)
	if chunkRatio == 0 {
		return nil, errors.New("vhdx: chunk ratio computed as 0 (bad sector/block size)")
	}
	payloadBlocks := PayloadBlockCount(info.VirtualDiskSize, info.BlockSize)
	if payloadBlocks == 0 {
		return nil, errors.New("vhdx: virtual disk size / block size implies zero payload blocks")
	}
	total := payloadBlocks + (payloadBlocks-1)/chunkRatio

	raw := make([]byte, total*8)
	if _, err := r.ReadAt(raw, int64(info.BATRegionOffset)); err != nil {
		return nil, fmt.Errorf("reading BAT (%d entries): %w", total, err)
	}

	entries := make([]BATEntry, total)
	for i := range entries {
		v := binary.LittleEndian.Uint64(raw[i*8 : i*8+8])
		entries[i] = BATEntry{
			State:        BATEntryState(v & 0x7),
			FileOffsetMB: v >> 20,
		}
	}
	return entries, nil
}
