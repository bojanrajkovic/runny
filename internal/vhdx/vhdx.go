//go:build windows

package vhdx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

	"github.com/Microsoft/go-winio/vhd"
)

// Fixed internal constants, not exposed as Convert parameters since nothing
// downstream needs them tunable. BlockSize is a free choice within
// [MS-VHDX] §2.6.2.1's 1-256 MiB range. The 512/512 sector sizes are
// verified against tart-format raw images' actual GPT sector-size
// assumption (the primary/backup GPT header signature lands at byte offset
// 512, not 4096) — declaring a mismatched value here would misinterpret
// every LBA offset in the guest's partition table.
const (
	blockSize          = 32 * 1024 * 1024
	logicalSectorSize  = 512
	physicalSectorSize = 512

	// minSourceSize is CreateVirtualDisk's undocumented minimum
	// MaximumSize — verified empirically on real hardware (2 MiB fails, 3
	// MiB succeeds, independent of BlockSizeInBytes); not stated anywhere
	// in Microsoft's docs. See internal/vhdx/CLAUDE.md.
	minSourceSize = 3 * 1024 * 1024

	// copyBufferSize sizes copyPayload's transfer buffer. The stdlib
	// io.Copy default (32 KiB) would drive millions of syscalls for a
	// real tens-of-GB disk image; a few MiB matches typical block-device
	// I/O sizing without holding an unreasonable amount of memory.
	copyBufferSize = 4 * 1024 * 1024
)

// backend is the subset of go-winio/vhd's calls Convert needs, seamed so
// tests can exercise Convert's control flow (success and every cleanup
// path) without a live elevated session or real disk allocation — same
// shape as internal/sysdaemon's scmMgr/scmService seam over the SCM.
type backend interface {
	createFixed(path string, maximumSize uint64) (syscall.Handle, error)
	attach(handle syscall.Handle) error
	physicalPath(handle syscall.Handle) (string, error)
	detach(handle syscall.Handle) error
	closeHandle(handle syscall.Handle) error
}

type winioBackend struct{}

// createFixed asks Windows' native virtual-disk API for a FIXED VHDX
// directly — CreateVirtualDiskFlagFullPhysicalAllocation is Microsoft's own
// documented mechanism for a fully-allocated, non-sparse VHDX ("used for
// the creation of a fixed VHD"). No SourcePath: that field only accepts
// another virtual disk or a physical disk device, never an arbitrary raw
// image file — the payload copy is ours to do.
func (winioBackend) createFixed(path string, maximumSize uint64) (syscall.Handle, error) {
	params := &vhd.CreateVirtualDiskParameters{
		Version: 2,
		Version2: vhd.CreateVersion2{
			MaximumSize:              maximumSize,
			BlockSizeInBytes:         blockSize,
			SectorSizeInBytes:        logicalSectorSize,
			PhysicalSectorSizeInByte: physicalSectorSize,
		},
	}
	return vhd.CreateVirtualDisk(path, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagFullPhysicalAllocation, params)
}

func (winioBackend) attach(handle syscall.Handle) error {
	return vhd.AttachVirtualDisk(handle, vhd.AttachVirtualDiskFlagNone, &vhd.AttachVirtualDiskParameters{Version: 2})
}

func (winioBackend) physicalPath(handle syscall.Handle) (string, error) {
	return vhd.GetVirtualDiskPhysicalPath(handle)
}

func (winioBackend) detach(handle syscall.Handle) error {
	return vhd.DetachVirtualDisk(handle)
}

func (winioBackend) closeHandle(handle syscall.Handle) error {
	return syscall.CloseHandle(handle)
}

// Convert produces a FIXED VHDX at dst from the raw disk image at src, via
// Windows' native virtual-disk API rather than a hand-rolled [MS-VHDX]
// writer: CreateVirtualDisk+FullPhysicalAllocation is spec-perfect by
// construction, and SourcePath can't ingest a raw file directly, so this
// function only asks the API to allocate a correctly-shaped fixed
// container and does the payload copy itself. dst must not already exist
// (Convert refuses rather than risk deleting a caller's unrelated file on
// a later failure — see convert's dst-exists check). src must be at least
// minSourceSize bytes (CreateVirtualDisk's own undocumented floor) and a
// multiple of the logical sector size (512). dst is removed if the payload
// was never fully written; a failure after a complete write (e.g. the
// trailing detach) is still returned as an error but leaves dst in place,
// since the VHDX itself is valid at that point.
func Convert(src, dst string) error {
	return convert(src, dst, winioBackend{})
}

func convert(src, dst string, b backend) (err error) {
	// Refusing here, before createFixed ever runs, means anything found at
	// dst on a later failure path was necessarily created by THIS call —
	// os.Remove(dst) below can never delete a file Convert didn't create.
	if _, statErr := os.Stat(dst); statErr == nil {
		return fmt.Errorf("destination %s already exists", dst)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking destination %s: %w", dst, statErr)
	}

	fi, statErr := os.Stat(src)
	if statErr != nil {
		return fmt.Errorf("stat source %s: %w", src, statErr)
	}
	if fi.Size() < minSourceSize {
		return fmt.Errorf("source %s is %d bytes, below CreateVirtualDisk's %d-byte minimum", src, fi.Size(), minSourceSize)
	}
	if fi.Size()%logicalSectorSize != 0 {
		return fmt.Errorf("source %s is %d bytes, not a multiple of the %d-byte sector size", src, fi.Size(), logicalSectorSize)
	}

	handle, createErr := b.createFixed(dst, uint64(fi.Size()))
	if createErr != nil {
		os.Remove(dst) // best-effort: CreateVirtualDisk may have left a partial file
		return fmt.Errorf("creating fixed VHDX %s: %w", dst, createErr)
	}

	written := false
	attached := false
	defer func() {
		var cleanupErr error
		if attached {
			if derr := b.detach(handle); derr != nil {
				cleanupErr = fmt.Errorf("detaching %s: %w", dst, derr)
			}
		}
		if cerr := b.closeHandle(handle); cerr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("closing VHDX handle: %w", cerr))
		}
		if !written {
			os.Remove(dst) // best-effort: no half-made VHDX left behind
		}
		err = errors.Join(err, cleanupErr)
	}()

	if attachErr := b.attach(handle); attachErr != nil {
		return fmt.Errorf("attaching %s: %w", dst, attachErr)
	}
	attached = true

	physPath, pathErr := b.physicalPath(handle)
	if pathErr != nil {
		return fmt.Errorf("resolving physical path for %s: %w", dst, pathErr)
	}

	if copyErr := copyPayload(src, physPath, fi.Size()); copyErr != nil {
		return fmt.Errorf("copying payload into %s: %w", dst, copyErr)
	}
	written = true
	return nil
}

// copyPayload streams src's bytes into the attached VHDX's block device.
// convert validates src's size is a multiple of the logical sector size
// before calling this, so every write lands sector-aligned. expectedSize
// is the size convert saw when it stat'd src; io.CopyBuffer alone would
// report success at whatever byte count src happens to reach EOF at, so a
// source that shrinks between that stat and this copy (or any other
// short read) would otherwise convert "successfully" into a VHDX with a
// valid prefix and a stale/zero tail — checking the count catches that
// instead of silently reporting success over an incomplete disk.
func copyPayload(src, devicePath string, expectedSize int64) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("opening source %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(devicePath, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("opening device %s: %w", devicePath, err)
	}
	buf := make([]byte, copyBufferSize)
	n, err := io.CopyBuffer(out, in, buf)
	if err != nil {
		out.Close()
		return fmt.Errorf("writing payload: %w", err)
	}
	if n != expectedSize {
		out.Close()
		return fmt.Errorf("copied %d bytes, want %d (source changed size during conversion)", n, expectedSize)
	}
	return out.Close()
}
