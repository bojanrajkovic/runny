# `.spike-winboot` — Windows-guest HCS boot + clone soak harness (throwaway spike)

Validation harness for the **"Windows boots on Windows"** workstream. **Not shipped.**
It lives in a `.`-prefixed directory, which Go (`go build ./...`) and Bazel/gazelle
both ignore, so it can never affect a build.

## What it proved (2026-07-23, on `tleilax`)

- **Spike A** — a Windows Server 2025 guest boots on runny's HCS backend using the
  shipped Linux boot recipe (`internal/vm/hcs_windows.go`) with **exactly two swaps**:
  the **Windows** Secure Boot template (`1734c6e8-3154-4dda-ba5f-a874cc483422`, vs the
  Linux MS-UEFI-CA anchor `272e7447-…`) and the eval VHDX as the SCSI disk.
- **Spike B / Phase-1** — HNS pre-commits the *correct* IP for a Windows guest
  (ARP-confirmed, **0 divergence across 4 concurrent boots** — unlike the intermittent
  Linux case); differencing clones (`go-winio/vhd.CreateDiffVhd`) of a Windows parent
  boot concurrently and each get a **unique IP**; boot-to-networked ~24s.

Full write-up + all decisions: Outline → *"Windows guests (Windows-on-Windows):
scoping & open decision tree."*

## Build & run

Cross-compile from macOS, run on the Windows host **elevated** (SSH as an admin
carries an elevated token):

```
GOOS=windows GOARCH=amd64 GOFLAGS=-mod=mod go build -o winboot.exe ./.spike-winboot/
scp winboot.exe <host>:C:/Temp/
# boot N differencing clones off a parent VHDX and ARP-soak them:
winboot.exe C:\Temp\ws2025-eval.vhdx 4
```

The eval VHDX is `aka.ms/WinServ2025vhd-enus` (~10.9 GB). The harness creates its
own differencing children, HNS endpoints, and compute systems, and cleans them up
on exit (compute systems + endpoints via `defer`; stray clone `.vhdx` files may need
a manual sweep).

## Reusable kernel (graduates into production)

The only production-bound bits are the **Windows Secure Boot template GUID** and the
confirmation that the boot doc needs only that + the disk swap for a Windows guest.
Those belong in `internal/vm/hcs_windows.go`'s per-OS path when the real backend gains
Windows-guest support. Everything else here — the PowerShell/`Get-NetNeighbor`/ARP
diagnostics and the clone fan-out — is spike-only; production reads the neighbor table
via `internal/vm`'s `GetIpNetTable2` glue and cycles guests through the FSM.
