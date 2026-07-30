//go:build windows

package vhdx

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Microsoft/go-winio/vhd"

	"github.com/bojanrajkovic/runny/internal/winhcs/security"
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

// backend is the subset of go-winio/vhd's calls Convert and
// CreateDifferencing need, seamed so tests can exercise their control flow
// (success and every cleanup path) without a live elevated session or real
// disk allocation — same shape as internal/sysdaemon's scmMgr/scmService
// seam over the SCM.
type backend interface {
	createFixed(path string, maximumSize uint64) (syscall.Handle, error)
	createDifferencing(child, parent string) error
	grantVmGroupAccess(path string, readWrite bool) error
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

// createDifferencing delegates to go-winio's own CreateDiffVhd rather than
// re-assembling CreateVirtualDiskParameters by hand (unlike createFixed,
// which needs FullPhysicalAllocation — a flag CreateDiffVhd doesn't expose).
// blockSizeInMB=0 asks CreateVirtualDisk for its native differencing
// default: verified empirically (both via New-VHD -Differencing and this
// exact call) at 2 MiB, independent of the parent's own block size — forcing
// createFixed's 32 MiB fixed-disk constant onto a differencing child isn't
// how Hyper-V itself sizes one. The result is confirmed NOT NTFS-sparse
// (fsutil sparse queryflag), matching the constraint CreateDifferencing's
// doc comment asserts.
func (winioBackend) createDifferencing(child, parent string) error {
	return vhd.CreateDiffVhd(child, parent, 0)
}

// grantVmGroupAccess grants the well-known "NT VIRTUAL MACHINE\Virtual
// Machines" group (SID S-1-5-83-0) access to path's DACL — the step Hyper-V's
// own tooling (New-VM, Add-VMHardDiskDrive) applies automatically and a bare
// HCS compute system never gets for free. Without it, Start fails with "The
// chain of virtual hard disks is inaccessible. The process has not been
// granted access rights to the parent virtual hard disk for the
// differencing disk." — confirmed against real hardware.
//
// readWrite distinguishes the parent from the child of a differencing pair:
// read-only is correct for the parent (the shared, immutable base a child
// only ever reads from), but the SAME read-only grant on the child — what
// hcsshim's own internal/computestorage helper applies to both base and diff
// VHD, for its own container-base-layer use case — reproduced the identical
// generic "Access is denied" on real hardware once the child was actually
// attached as a running VM's writable boot disk: it's the disk every guest
// write lands on. Full Control (matching Microsoft's own documented manual
// fix-up for a Hyper-V VM's disk files) was the only thing that fixed it.
func (winioBackend) grantVmGroupAccess(path string, readWrite bool) error {
	if readWrite {
		return security.GrantVmGroupAccessWithMask(path, security.AccessMaskAll)
	}
	return security.GrantVmGroupAccess(path)
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

// CreateDifferencing creates a differencing VHDX at child whose parent is
// parent — the per-slot ephemeral-disk clone for the windows host path, the
// APFS-clonefile analog: near-instant regardless of parent size, since a
// fresh differencing child carries no payload blocks of its own, only a
// live block-level dependency on parent (see internal/images/prune.go's
// parent-reference check, which exists because of that dependency). A
// single bounded syscall, no polling. child must NOT end up NTFS-sparse —
// Hyper-V rejects sparse VHDX files — and it doesn't: go-winio's
// CreateDiffVhd sets no sparse flag, confirmed empirically (see
// internal/vhdx/CLAUDE.md). Also grants the VM group access to both parent
// (read) and child (read-write) — without it, a bare compute system's Start
// fails once it tries to actually attach the chain.
func CreateDifferencing(child, parent string) error {
	return createDifferencing(child, parent, winioBackend{})
}

func createDifferencing(child, parent string, b backend) error {
	if err := b.createDifferencing(child, parent); err != nil {
		return err
	}
	if err := b.grantVmGroupAccess(parent, false); err != nil {
		return fmt.Errorf("granting VM group read access to parent %s: %w", parent, err)
	}
	if err := b.grantVmGroupAccess(child, true); err != nil {
		return fmt.Errorf("granting VM group read-write access to child %s: %w", child, err)
	}
	return nil
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
// multiple of the logical sector size (512).
//
// Convert builds the whole disk at a temp path next to dst and renames it
// into place only once the payload is fully written, then — dst's mere
// existence must be a reliable completeness signal for callers (internal/
// tart.CloneVHDX checks it before differencing-cloning; internal/tart.
// Bundle.Verify treats it as "conversion complete"), and createFixed's
// CreateVirtualDiskFlagFullPhysicalAllocation allocates the full file size
// up front, well before the payload copy finishes — so without the rename, a
// concurrent Stat mid-conversion would see a complete-looking but
// not-yet-written file. dst is never touched until that final rename; the
// temp file is removed if the payload was never fully written.
func Convert(src, dst string) error {
	return convert(src, dst, winioBackend{})
}

// convertingTempPath is dst's in-progress conversion path. The marker goes
// before the extension, not after: CreateVirtualDisk selects its provider by
// file extension, and a path ending in ".converting" instead of ".vhdx"
// fails with "A virtual disk support provider for the specified file was not
// found" — confirmed against real hardware. fakeBackend never
// calls the real Win32 API, so this was untestable until then.
func convertingTempPath(dst string) string {
	ext := filepath.Ext(dst)
	return strings.TrimSuffix(dst, ext) + ".converting" + ext
}

func convert(src, dst string, b backend) error {
	// Refusing here, before anything is created, means a caller can trust
	// dst's post-Convert existence unconditionally — Convert never
	// overwrites or races a file already at dst.
	if _, statErr := os.Stat(dst); statErr == nil {
		return fmt.Errorf("destination %s already exists", dst)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("checking destination %s: %w", dst, statErr)
	}

	tmp := convertingTempPath(dst)
	written, convErr := convertToTemp(src, tmp, b)
	if !written {
		// The payload itself never completed; convertToTemp already cleaned
		// up tmp, so there's nothing to rename.
		return convErr
	}
	// The payload is valid the moment written is true, even if convErr
	// carries a trailing detach/close-only failure — preserve the pre-
	// existing guarantee that a fully-written disk survives a cleanup-only
	// error, by renaming regardless and joining any rename error with it.
	// This can't reopen the race Convert exists to close: convertToTemp has
	// already fully returned (its own defer, including detach/close, ran
	// before this point), so dst only ever appears once the payload — and
	// every cleanup attempt on it — is finished, never mid-copy.
	if err := os.Rename(tmp, dst); err != nil {
		return errors.Join(convErr, fmt.Errorf("finalizing %s: %w", dst, err))
	}
	return convErr
}

// convertToTemp does the actual CreateVirtualDisk+attach+copy+detach+close
// sequence into tmp, reporting whether the payload copy itself completed
// (written) independent of any trailing cleanup error in err — convert
// decides whether to rename tmp into place based on written, not on
// whether err is nil. Split out from convert so the handle is fully
// detached and closed (this function's own defer) before convert ever
// touches tmp again — a VHDX still attached to this process can't be
// reliably renamed.
func convertToTemp(src, tmp string, b backend) (written bool, err error) {
	fi, statErr := os.Stat(src)
	if statErr != nil {
		return false, fmt.Errorf("stat source %s: %w", src, statErr)
	}
	if fi.Size() < minSourceSize {
		return false, fmt.Errorf("source %s is %d bytes, below CreateVirtualDisk's %d-byte minimum", src, fi.Size(), minSourceSize)
	}
	if fi.Size()%logicalSectorSize != 0 {
		return false, fmt.Errorf("source %s is %d bytes, not a multiple of the %d-byte sector size", src, fi.Size(), logicalSectorSize)
	}
	// A prior aborted Convert may have left a stale temp file; clear it so
	// createFixed doesn't fail EEXIST against our own leftover.
	if rmErr := os.Remove(tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return false, fmt.Errorf("clearing stale %s: %w", tmp, rmErr)
	}

	handle, createErr := b.createFixed(tmp, uint64(fi.Size()))
	if createErr != nil {
		os.Remove(tmp) // best-effort: CreateVirtualDisk may have left a partial file
		return false, fmt.Errorf("creating fixed VHDX %s: %w", tmp, createErr)
	}

	attached := false
	defer func() {
		var cleanupErr error
		if attached {
			if derr := b.detach(handle); derr != nil {
				cleanupErr = fmt.Errorf("detaching %s: %w", tmp, derr)
			}
		}
		if cerr := b.closeHandle(handle); cerr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("closing VHDX handle: %w", cerr))
		}
		if !written {
			os.Remove(tmp) // best-effort: no half-made VHDX left behind
		}
		err = errors.Join(err, cleanupErr)
	}()

	if attachErr := b.attach(handle); attachErr != nil {
		return false, fmt.Errorf("attaching %s: %w", tmp, attachErr)
	}
	attached = true

	physPath, pathErr := b.physicalPath(handle)
	if pathErr != nil {
		return false, fmt.Errorf("resolving physical path for %s: %w", tmp, pathErr)
	}

	if copyErr := copyPayload(src, physPath, fi.Size()); copyErr != nil {
		return false, fmt.Errorf("copying payload into %s: %w", tmp, copyErr)
	}
	return true, nil
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
