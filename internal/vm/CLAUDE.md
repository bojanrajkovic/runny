# internal/vm — AI Agent Notes

`Manager`/`Machine` (`vm.go`) boot tart-format bundles in-process: `VZManager`/`vzMachine`
(`vz_darwin.go`) on Virtualization.framework, `HCSManager`/`hcsMachine`
(`hcs_windows.go`) on bare Hyper-V compute systems. `stop.go`'s bounded
graceful-then-force sequence is platform-independent and shared by both. See
ADR-0026 for the Hyper-V backend's decisions and why; this doc is sharp edges only.

## Sharp edges

- **The HNS neighbor-table entry does not clear itself when the endpoint is
  deleted.** Confirmed empirically across three separate boot cycles: the
  `Permanent` entry `WaitIP` reads (`neighbortable_windows.go`) outlived
  `Terminate`, the HNS endpoint's own `Delete`, and 30s of polling after both.
  `hcsMachine.Stop`'s `destroy()` scrubs it explicitly (`deleteNeighborEntry`)
  once the guest is confirmed stopped — skip this and entries accumulate one
  stale row per boot cycle, bounded only by the Default Switch's MAC pool size.
- **`MIB_IPNET_ROW2`/`SOCKADDR_INET` are hand-laid-out, not generated.**
  `x/sys/windows` has no binding for this corner of `iphlpapi`, and mkwinsyscall
  wouldn't build cleanly against this repo's vendored copy — the struct fields,
  order, and padding in `neighbortable_windows.go` are transcribed directly from
  Microsoft Learn's `MIB_IPNET_ROW2`/`SOCKADDR_INET` pages (cited inline), not
  reconstructed from memory. If Microsoft ever adds fields to either struct,
  re-verify against the current docs before touching offsets.
- **No console-interaction networking fallback exists, on purpose.** The
  validated guest image (`ghcr.io/cirruslabs/ubuntu-runner-amd64`) self-configures
  eth0 via cloud-init's own dynamically-generated netplan — confirmed end-to-end
  (`Boot`→`WaitIP`→TCP:22 reachable) with zero console interaction. If a future
  image needs a live DHCP fixup, `WaitIP` will simply time out; that's the signal
  to build one, not something pre-built speculatively here (see ADR-0026's
  rejected alternatives).
- **`Boot` never calls `vhdx.CreateDifferencing` itself.** The slot's
  differencing-child VHDX is already there at `bundle.VHDXPath()` by the time
  `HCSManager.Boot` runs — `internal/tart.CloneVHDX` creates it during the FSM's
  CLONE state, mirroring darwin's `Clone`/clonefile split. `Boot` only attaches it.
- **Guest arch validation is host-relative, not hardcoded.** `hcs_windows.go`/
  `vz_darwin.go` each reject a bundle whose `Arch` doesn't match this process's own
  `runtime.GOARCH` — neither Hyper-V nor Virtualization.framework cross-emulates
  architectures (Rosetta 2 translates *userspace binaries* inside an already-arm64
  guest; it never lets VZ boot an amd64 kernel). `Bundle.LoadConfig` itself stays a
  portable shape check with no host-arch opinion, so it parses identically on any
  CI runner regardless of that runner's own `GOARCH`.
