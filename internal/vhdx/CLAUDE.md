# internal/vhdx — AI Agent Notes

Converts a raw disk image into a FIXED (fully-allocated) VHDX via Windows' native virtual-disk API — `CreateVirtualDisk`+`CreateVirtualDiskFlagFullPhysicalAllocation`, `AttachVirtualDisk`, a plain byte copy into the resulting block device, `DetachVirtualDisk` — not a hand-rolled [MS-VHDX] writer. `vhdx.go` (`Convert`, `CreateDifferencing`) is windows-only (`//go:build windows`); `reader.go`/`parentlocator.go` (the structural parser, and the differencing-disk parent-locator resolver built on it) have no Windows dependency and build everywhere.

`CreateDifferencing` is the per-slot ephemeral-disk clone for the windows host path — the APFS-clonefile analog (`internal/clonefile`, `internal/tart.Clone` on darwin). `ParentLocator` is what `internal/images.PlanImageBundlePrune`'s parent-reference check (`referenced` parameter) uses to keep a differencing child's parent bundle from being pruned out from under a running guest.

## Sharp edges

- **`CreateVirtualDisk`'s `MaximumSize` has an undocumented minimum of 3 MiB.** Anything smaller fails with the generic `ERROR_INVALID_PARAMETER` ("The parameter is incorrect"), regardless of `BlockSizeInBytes` — verified empirically on real hardware (2 MiB fails, 3 MiB succeeds, independent of block size). Not stated anywhere in Microsoft's `CreateVirtualDisk`/`CREATE_VIRTUAL_DISK_PARAMETERS` docs. Any future minimum-size validation in `Convert` needs to account for this floor, not just [MS-VHDX]'s own block-size/sector-size constraints. `CreateDifferencing` is unaffected — it never sets `MaximumSize`; a differencing child inherits its virtual size from the parent.
- **`SourcePath` cannot ingest a raw disk image.** It only accepts another virtual disk or a physical disk device (`\\.\PhysicalDriveN`) — confirmed against `CreateVirtualDisk`'s remarks and `Convert-VHD`'s docs, both of which require an existing `.vhd`/`.vhdx` as input. The raw-to-VHDX payload copy is always this package's own `io.Copy`, never a single native call.
- **A differencing child's default block size is 2 MiB, independent of its parent's block size.** Verified empirically on real hardware: a 1 MiB-block parent produced a 2 MiB-block child under both `New-VHD -Differencing` (no `-BlockSizeBytes`) and this package's own `vhd.CreateDiffVhd(child, parent, 0)`. `CreateDifferencing` passes `blockSizeInMB=0` deliberately, to get Hyper-V's own native differencing default rather than forcing `Convert`'s 32 MiB fixed-disk constant onto a structurally different disk type.
- **A real Hyper-V-authored `absolute_win32_path` parent-locator entry does NOT carry the spec's stated `\\?\` prefix.** [MS-VHDX] §2.6.2.6.3 says an implementation "MUST" write that prefix; real output (`New-VHD -Differencing` and this package's own `CreateDifferencing`, both checked against raw bytes) omits it — `C:\spike-306\fixed-min.vhdx`, not `\\?\C:\spike-306\fixed-min.vhdx`. `ParentLocator` does not require or validate the prefix; doing so would reject files real Hyper-V wrote.
- **`parent_linkage2` is present-but-zero-GUID in real output, not omitted.** Hyper-V always writes all five well-known parent-locator keys, even when only `parent_linkage` (required) and one path key are meaningful. `ParentLocator` doesn't special-case this — an all-zero GUID never matches a real `DataWriteGuid` anyway.
- **A differencing child is confirmed NOT NTFS-sparse** (`fsutil sparse queryflag`), matching `CreateDifferencing`'s doc comment — verified for both `New-VHD -Differencing` and `vhd.CreateDiffVhd` directly, on real hardware.

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

## Regenerating `testdata/differencing-min.vhdx`

A differencing child of `fixed-min.vhdx` itself — 4,194,304 bytes (4 MiB): no payload blocks (a fresh differencing child has none), so it's almost entirely header+region+metadata. Parented deliberately to the checked-in `fixed-min.vhdx` rather than a second fixture, to avoid a second binary blob in git-lfs for no added coverage.

**The parent file on the Windows host MUST be named `fixed-min.vhdx`**, sitting next to the child — `New-VHD -Differencing` bakes the parent's filename into the child's `relative_path` parent-locator entry (`.\fixed-min.vhdx`), and `TestParentLocator_RealFixture` resolves against that exact relative path. A parent named anything else produces a fixture that fails the test.

```powershell
Copy-Item <path-to-checked-in-fixed-min.vhdx> C:\some-dir\fixed-min.vhdx
New-VHD -Path C:\some-dir\differencing-min.vhdx -Differencing -ParentPath C:\some-dir\fixed-min.vhdx
```

Copy the result to `internal/vhdx/testdata/differencing-min.vhdx` and `git add` it.
