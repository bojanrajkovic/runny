package vhdx

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"math"
	"testing"

	"github.com/Microsoft/go-winio/pkg/guid"
)

func TestChunkRatioAndPayloadBlockCount(t *testing.T) {
	cases := []struct {
		name                  string
		virtualDiskSize       uint64
		blockSize             uint32
		logicalSectorSize     uint32
		wantChunkRatio        uint64
		wantPayloadBlockCount uint64
	}{
		{"exact multiple of block size", 4 * 1024 * 1024, 1024 * 1024, 512, 4096, 4},
		{"partial final block", 4*1024*1024 + 1, 1024 * 1024, 512, 4096, 5},
		{"locked 32MiB/512 choice, 40GB image", 40_000_000_000, 32 * 1024 * 1024, 512, 128, 1193},
		{"locked 32MiB/512 choice, 150GiB image, multi-chunk", 150 * 1024 * 1024 * 1024, 32 * 1024 * 1024, 512, 128, 4800},
		{"4Kn sector size halves chunk ratio", 4 * 1024 * 1024, 1024 * 1024, 4096, 32768, 4},
		{"zero block size returns 0, not a divide-by-zero panic", 4 * 1024 * 1024, 0, 512, 0, 0},
		{"virtual disk size near uint64 max doesn't overflow to a small answer", math.MaxUint64, 1024 * 1024, 512, 4096, 17592186044416},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ChunkRatio(c.logicalSectorSize, c.blockSize); got != c.wantChunkRatio {
				t.Errorf("ChunkRatio(%d, %d) = %d, want %d", c.logicalSectorSize, c.blockSize, got, c.wantChunkRatio)
			}
			if got := PayloadBlockCount(c.virtualDiskSize, c.blockSize); got != c.wantPayloadBlockCount {
				t.Errorf("PayloadBlockCount(%d, %d) = %d, want %d", c.virtualDiskSize, c.blockSize, got, c.wantPayloadBlockCount)
			}
		})
	}
}

// buildFixture assembles a minimal but spec-shaped VHDX byte buffer: file
// identifier, one region-table copy (BAT + Metadata regions, with a real
// checksum since Read validates it) a metadata table with the four items
// Read surfaces, and a small BAT. Small enough (blockSize/virtualDiskSize
// chosen so chunkRatio vastly exceeds payloadBlockCount) that no
// sector-bitmap slot is interleaved — ReadBAT's entry count is then
// exactly payloadBlockCount, keeping the fixture's BAT section a plain,
// hand-verifiable list. This is a TEST fixture builder, not a VHDX writer:
// no log or secondary region-table copy is produced, since Read doesn't
// need either to parse a fixture whose primary copy is already valid.
func buildFixture(t *testing.T, blockSize, virtualDiskSize uint64, batOffsets []uint64) []byte {
	t.Helper()

	const (
		metadataRegionOffset = 2 * 1024 * 1024
		metadataRegionLength = 128 * 1024
		batRegionOffset      = 3 * 1024 * 1024
		itemDataBase         = 64 * 1024 // §2.6.1.2: item Offset MUST be >= 64KiB, relative to region start
	)

	buf := make([]byte, batRegionOffset+8*int64(len(batOffsets)))
	copy(buf[0:8], "vhdxfile")

	// Region table @ 192KiB: header (16B) + 2 entries (32B each).
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
	writeRegionEntry(0, regionBAT, batRegionOffset, uint32(8*len(batOffsets)))
	writeRegionEntry(1, regionMetadata, metadataRegionOffset, metadataRegionLength)
	// Checksum field (rt[4:8]) is still zero here, matching §2.2.3.1's
	// zero-then-compute procedure -- Read now verifies this, so a fixture
	// with a zero/wrong checksum would be rejected as corrupt.
	binary.LittleEndian.PutUint32(rt[4:8], crc32.Checksum(rt, castagnoliTable))

	// Metadata table @ metadataRegionOffset: header (32B) + 4 entries (32B each).
	mt := buf[metadataRegionOffset : metadataRegionOffset+metadataRegionLength]
	copy(mt[0:8], "metadata")
	binary.LittleEndian.PutUint16(mt[10:12], 4)
	writeItemEntry := func(idx int, g guid.GUID, itemOffset, itemLength uint32) {
		off := uint32(32 + idx*32)
		raw := g.ToWindowsArray()
		copy(mt[off:off+16], raw[:])
		binary.LittleEndian.PutUint32(mt[off+16:off+20], itemOffset)
		binary.LittleEndian.PutUint32(mt[off+20:off+24], itemLength)
	}
	writeItemEntry(0, itemFileParameters, itemDataBase, 8)
	writeItemEntry(1, itemVirtualDiskSize, itemDataBase+8, 8)
	writeItemEntry(2, itemLogicalSectorSize, itemDataBase+16, 4)
	writeItemEntry(3, itemPhysicalSectorSize, itemDataBase+20, 4)

	binary.LittleEndian.PutUint32(mt[itemDataBase:itemDataBase+4], uint32(blockSize))
	binary.LittleEndian.PutUint32(mt[itemDataBase+4:itemDataBase+8], 0x1) // LeaveBlockAllocated=1, HasParent=0
	binary.LittleEndian.PutUint64(mt[itemDataBase+8:itemDataBase+16], virtualDiskSize)
	binary.LittleEndian.PutUint32(mt[itemDataBase+16:itemDataBase+20], 512)
	binary.LittleEndian.PutUint32(mt[itemDataBase+20:itemDataBase+24], 512)

	// BAT @ batRegionOffset: one FULLY_PRESENT entry per offset given.
	for i, mb := range batOffsets {
		v := uint64(StateFullyPresent) | (mb << 20)
		binary.LittleEndian.PutUint64(buf[batRegionOffset+int64(i)*8:], v)
	}

	return buf
}

func TestRead(t *testing.T) {
	const blockSize = 1024 * 1024 // 1 MiB: chunkRatio (2048) >> 4 payload blocks, no sector-bitmap slot in range
	virtualDiskSize := uint64(3*1024*1024 + 512*1024)
	buf := buildFixture(t, blockSize, virtualDiskSize, []uint64{10, 11, 12, 13})

	info, err := Read(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if info.BlockSize != blockSize {
		t.Errorf("BlockSize = %d, want %d", info.BlockSize, blockSize)
	}
	if !info.LeaveBlockAllocated {
		t.Error("LeaveBlockAllocated = false, want true")
	}
	if info.HasParent {
		t.Error("HasParent = true, want false")
	}
	if info.VirtualDiskSize != virtualDiskSize {
		t.Errorf("VirtualDiskSize = %d, want %d", info.VirtualDiskSize, virtualDiskSize)
	}
	if info.LogicalSectorSize != 512 || info.PhysicalSectorSize != 512 {
		t.Errorf("sector sizes = %d/%d, want 512/512", info.LogicalSectorSize, info.PhysicalSectorSize)
	}
}

func TestReadBAT(t *testing.T) {
	const blockSize = 1024 * 1024
	virtualDiskSize := uint64(3*1024*1024 + 512*1024) // Ceil -> 4 payload blocks
	offsets := []uint64{10, 11, 12, 13}
	buf := buildFixture(t, blockSize, virtualDiskSize, offsets)

	r := bytes.NewReader(buf)
	info, err := Read(r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	entries, err := ReadBAT(r, info)
	if err != nil {
		t.Fatalf("ReadBAT: %v", err)
	}
	if len(entries) != len(offsets) {
		t.Fatalf("len(entries) = %d, want %d", len(entries), len(offsets))
	}
	for i, e := range entries {
		if e.State != StateFullyPresent {
			t.Errorf("entries[%d].State = %d, want StateFullyPresent", i, e.State)
		}
		if e.FileOffsetMB != offsets[i] {
			t.Errorf("entries[%d].FileOffsetMB = %d, want %d", i, e.FileOffsetMB, offsets[i])
		}
	}
}

func TestReadBAT_ZeroVirtualDiskSize(t *testing.T) {
	buf := buildFixture(t, 1024*1024, 0, nil)
	r := bytes.NewReader(buf)
	info, err := Read(r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if _, err := ReadBAT(r, info); err == nil {
		t.Fatal("ReadBAT with VirtualDiskSize=0: want error (zero payload blocks), got nil")
	}
}

func TestRead_ZeroBlockSize(t *testing.T) {
	// buildFixture writes blockSize straight into the File Parameters
	// item's BlockSize field -- this crafts a VHDX carrying the exact
	// value that would divide-by-zero in ChunkRatio if Read let it
	// through, proving the parse-time rejection actually fires on real
	// (if malformed) VHDX bytes, not just via a direct function call.
	buf := buildFixture(t, 0, 4*1024*1024, []uint64{10, 11, 12, 13})
	if _, err := Read(bytes.NewReader(buf)); err == nil {
		t.Fatal("Read with BlockSize=0: want error, got nil")
	}
}

func TestRead_CorruptRegionTableChecksum(t *testing.T) {
	buf := buildFixture(t, 1024*1024, 4*1024*1024, []uint64{10, 11, 12, 13})
	// Flip a byte inside the primary region table's entries, after the
	// checksum was already computed over the original bytes -- the
	// secondary copy (never populated by buildFixture) is equally invalid,
	// so this must surface as a hard error, not silently wrong offsets.
	buf[regionTable1Offset+20] ^= 0xFF
	if _, err := Read(bytes.NewReader(buf)); err == nil {
		t.Fatal("Read with a corrupted region table: want error, got nil")
	}
}

func TestRead_MetadataItemLengthExceedsRegion(t *testing.T) {
	buf := buildFixture(t, 1024*1024, 4*1024*1024, []uint64{10, 11, 12, 13})
	// The File Parameters item entry is the metadata table's first entry,
	// at metadataRegionOffset+32; its Length field is the 4 bytes at +20.
	const metadataRegionOffset = 2 * 1024 * 1024
	binary.LittleEndian.PutUint32(buf[metadataRegionOffset+32+20:], 0xFFFFFFFF)
	if _, err := Read(bytes.NewReader(buf)); err == nil {
		t.Fatal("Read with an out-of-bounds metadata item length: want error, got nil")
	}
}

func TestRead_NotAVHDX(t *testing.T) {
	_, err := Read(bytes.NewReader(make([]byte, 4096)))
	if !errors.Is(err, ErrNotAVHDX) {
		t.Fatalf("Read(garbage) = %v, want ErrNotAVHDX", err)
	}
}
