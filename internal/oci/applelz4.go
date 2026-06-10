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
// CRITICAL: blocks are NOT independent. Apple's encoder is a streaming LZ4
// whose matches reach back across block boundaries into the previous 64KB of
// *decoded* output. Decoding a block requires the tail of what came before as
// an LZ4 dictionary — without it, the first back-reference fails (this bit us
// against real ghcr layers; the testdata fixture is real compression_tool
// output and would catch a regression).
//
// pierrec/lz4 only speaks the standard frame, so this file decodes Apple's
// framing over its block-level dict API.

var (
	magicCompressed = [4]byte{'b', 'v', '4', '1'}
	magicRaw        = [4]byte{'b', 'v', '4', '-'}
	magicEnd        = [4]byte{'b', 'v', '4', '$'}
)

// lz4Window is the LZ4 match window: how much decoded history a block may
// reference.
const lz4Window = 64 << 10

// appleLZ4Decode streams src (Apple-framed LZ4) into w, returning bytes
// written. maxBlock guards allocation against corrupt headers.
func appleLZ4Decode(w io.Writer, src io.Reader) (int64, error) {
	const maxBlock = 64 << 20
	var written int64
	var dict []byte // tail of decoded output, ≤ lz4Window bytes
	var magic [4]byte

	emit := func(b []byte) error {
		if _, err := w.Write(b); err != nil {
			return err
		}
		written += int64(len(b))
		// Maintain the rolling dictionary.
		switch {
		case len(b) >= lz4Window:
			dict = append(dict[:0], b[len(b)-lz4Window:]...)
		case len(dict)+len(b) <= lz4Window:
			dict = append(dict, b...)
		default:
			keep := lz4Window - len(b)
			dict = append(dict[:0], append(dict[len(dict)-keep:len(dict):len(dict)], b...)...)
		}
		return nil
	}

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
			n, err := lz4.UncompressBlockWithDict(comp, dst, dict)
			if err != nil {
				return written, fmt.Errorf("applelz4: decompressing block: %w", err)
			}
			if uint32(n) != dSize {
				return written, fmt.Errorf("applelz4: block decompressed to %d, header said %d", n, dSize)
			}
			if err := emit(dst[:n]); err != nil {
				return written, err
			}
		case magicRaw:
			var hdr [4]byte
			if _, err := io.ReadFull(src, hdr[:]); err != nil {
				return written, fmt.Errorf("applelz4: reading raw header: %w", err)
			}
			size := binary.LittleEndian.Uint32(hdr[:])
			if size > maxBlock {
				return written, fmt.Errorf("applelz4: implausible raw block size %d", size)
			}
			raw := make([]byte, size)
			if _, err := io.ReadFull(src, raw); err != nil {
				return written, fmt.Errorf("applelz4: reading raw block: %w", err)
			}
			if err := emit(raw); err != nil {
				return written, err
			}
		default:
			return written, fmt.Errorf("applelz4: unknown block magic %q", magic)
		}
	}
}
