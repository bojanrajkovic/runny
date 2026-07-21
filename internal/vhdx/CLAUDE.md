# internal/vhdx — AI Agent Notes

Converts a raw disk image into a FIXED (fully-allocated) VHDX via Windows' native virtual-disk API — `CreateVirtualDisk`+`CreateVirtualDiskFlagFullPhysicalAllocation`, `AttachVirtualDisk`, a plain byte copy into the resulting block device, `DetachVirtualDisk` — not a hand-rolled [MS-VHDX] writer. `vhdx.go` (`Convert`) is windows-only (`//go:build windows`); `reader.go` (the structural parser used to verify `Convert`'s output, and reusable for differencing-disk parent-locator work) has no Windows dependency and builds everywhere.

## Sharp edges

- **`CreateVirtualDisk`'s `MaximumSize` has an undocumented minimum of 3 MiB.** Anything smaller fails with the generic `ERROR_INVALID_PARAMETER` ("The parameter is incorrect"), regardless of `BlockSizeInBytes` — verified empirically on real hardware (2 MiB fails, 3 MiB succeeds, independent of block size). Not stated anywhere in Microsoft's `CreateVirtualDisk`/`CREATE_VIRTUAL_DISK_PARAMETERS` docs. Any future minimum-size validation in `Convert` needs to account for this floor, not just [MS-VHDX]'s own block-size/sector-size constraints.
- **`SourcePath` cannot ingest a raw disk image.** It only accepts another virtual disk or a physical disk device (`\\.\PhysicalDriveN`) — confirmed against `CreateVirtualDisk`'s remarks and `Convert-VHD`'s docs, both of which require an existing `.vhd`/`.vhdx` as input. The raw-to-VHDX payload copy is always this package's own `io.Copy`, never a single native call.

## Regenerating `testdata/fixed-min.vhdx`

The smallest fixed VHDX `CreateVirtualDisk` will produce — 7,340,032 bytes (7 MiB): 1 MiB header section + 1 MiB log region + 1 MiB metadata region + 1 MiB BAT region + 3 payload blocks (1 MiB each, the spec minimum block size), every payload BAT entry `PAYLOAD_BLOCK_FULLY_PRESENT`. Tracked via git-lfs (`internal/vhdx/testdata/*.vhdx` in `.gitattributes`) — regenerating a binary fixture adds a full new copy to git history, so these aren't committed as plain blobs.

To regenerate on a real Windows host, with `github.com/Microsoft/go-winio/vhd` on the build path: create a 3 MiB all-zero source file, then run exactly what `Convert` does, with the smallest legal parameters:

```go
params := &vhd.CreateVirtualDiskParameters{
    Version: 2,
    Version2: vhd.CreateVersion2{
        MaximumSize:              3 * 1024 * 1024, // the empirical floor above
        BlockSizeInBytes:         1024 * 1024,     // spec minimum -- smallest possible payload region
        SectorSizeInBytes:        512,
        PhysicalSectorSizeInByte: 512,
    },
}
handle, err := vhd.CreateVirtualDisk(dst, vhd.VirtualDiskAccessNone, vhd.CreateVirtualDiskFlagFullPhysicalAllocation, params)
// ...
err = vhd.AttachVirtualDisk(handle, vhd.AttachVirtualDiskFlagNone, &vhd.AttachVirtualDiskParameters{Version: 2})
physPath, err := vhd.GetVirtualDiskPhysicalPath(handle) // \\.\PhysicalDriveN
// io.Copy the 3 MiB zero-filled source into physPath, then:
err = vhd.DetachVirtualDisk(handle)
err = syscall.CloseHandle(handle)
```

Copy the result to `internal/vhdx/testdata/fixed-min.vhdx` and `git add` it — the LFS clean filter (mise-managed `git-lfs`, repo-scoped hooks via `git lfs install --local`) handles the rest.
