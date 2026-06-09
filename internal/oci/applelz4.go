package oci

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/pierrec/lz4/v4"
)

// tart compresses disk.v2 layers with Apple's Compression framework, whose
// LZ4 output is NOT the standard LZ4 frame format: it is a sequence of
// self-framed blocks —
//
//	"bv41" | uint32le decompressedSize | uint32le compressedSize | lz4 block
//	"bv4-" | uint32le size             | raw bytes (incompressible block)
//	"bv4$"                                                       (stream end)
//
// pierrec/lz4 only speaks the standard frame, so this file decodes Apple's
// framing over its block-level API.

var (
	magicCompressed = [4]byte{'b', 'v', '4', '1'}
	magicRaw        = [4]byte{'b', 'v', '4', '-'}
	magicEnd        = [4]byte{'b', 'v', '4', '$'}
)

// appleLZ4Decode streams src (Apple-framed LZ4) into w, returning bytes
// written. maxBlock guards allocation against corrupt headers.
func appleLZ4Decode(w io.Writer, src io.Reader) (int64, error) {
	const maxBlock = 64 << 20
	var written int64
	var magic [4]byte
	for {
		if _, err := io.ReadFull(src, magic[:]); err != nil {
			if errors.Is(err, io.EOF) {
				// Tolerate a missing end marker: some encoders stop at EOF.
				return written, nil
			}
			return written, fmt.Errorf("applelz4: reading block magic: %w", err)
		}
		switch magic {
		case magicEnd:
			return written, nil
		case magicCompressed:
			var hdr [8]byte
			if _, err := io.ReadFull(src, hdr[:]); err != nil {
				return written, fmt.Errorf("applelz4: reading block header: %w", err)
			}
			dSize := binary.LittleEndian.Uint32(hdr[0:4])
			cSize := binary.LittleEndian.Uint32(hdr[4:8])
			if dSize > maxBlock || cSize > maxBlock {
				return written, fmt.Errorf("applelz4: implausible block sizes d=%d c=%d", dSize, cSize)
			}
			comp := make([]byte, cSize)
			if _, err := io.ReadFull(src, comp); err != nil {
				return written, fmt.Errorf("applelz4: reading compressed block: %w", err)
			}
			dst := make([]byte, dSize)
			n, err := lz4.UncompressBlock(comp, dst)
			if err != nil {
				return written, fmt.Errorf("applelz4: decompressing block: %w", err)
			}
			if uint32(n) != dSize {
				return written, fmt.Errorf("applelz4: block decompressed to %d, header said %d", n, dSize)
			}
			if _, err := w.Write(dst[:n]); err != nil {
				return written, err
			}
			written += int64(n)
		case magicRaw:
			var hdr [4]byte
			if _, err := io.ReadFull(src, hdr[:]); err != nil {
				return written, fmt.Errorf("applelz4: reading raw header: %w", err)
			}
			size := binary.LittleEndian.Uint32(hdr[:])
			if size > maxBlock {
				return written, fmt.Errorf("applelz4: implausible raw block size %d", size)
			}
			if n, err := io.CopyN(w, src, int64(size)); err != nil {
				return written + n, fmt.Errorf("applelz4: copying raw block: %w", err)
			}
			written += int64(size)
		default:
			return written, fmt.Errorf("applelz4: unknown block magic %q", magic)
		}
	}
}
