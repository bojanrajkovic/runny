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
  A divergent boot leaves **more than one** `Permanent` row for the MAC (stale
  pre-commit plus real lease), so `scrubNeighborEntry` deletes *every* match
  (`permanentEntriesForMAC`), not just the first — a single-delete scrub leaks
  the rest.
- **`MIB_IPNET_ROW2`/`SOCKADDR_INET` are hand-laid-out, not generated.**
  `x/sys/windows` has no binding for this corner of `iphlpapi`, and mkwinsyscall
  wouldn't build cleanly against this repo's vendored copy — the struct fields,
  order, and padding in `neighbortable_windows.go` are transcribed directly from
  Microsoft Learn's `MIB_IPNET_ROW2`/`SOCKADDR_INET` pages (cited inline), not
  reconstructed from memory. If Microsoft ever adds fields to either struct,
  re-verify against the current docs before touching offsets.
- **A console-interaction networking fallback exists for Linux guests, and is
  load-bearing for the currently-validated image.** (Windows guests take a
  different `WaitIP` path entirely — see the last bullet below.) The validated
  Linux guest image
  (`ghcr.io/cirruslabs/ubuntu-runner-amd64`) does NOT self-configure `eth0` on
  this backend: its baked netplan matches interface names `en*`, which
  hv_netvsc's always-`eth0` naming never satisfies, so `eth0` sits down and
  DHCP never even starts without help — confirmed on real hardware from a
  from-scratch differencing-disk boot with zero prior state, and again on a
  plain classic Hyper-V VM. `hcsMachine.WaitIP` (`hcs_windows.go`) gives the
  guest a bounded grace period to prove it can self-configure, then falls back
  once to `fixupNetwork` (`netfixup_windows.go`): dial the HCS console pipe,
  log in, and apply a netplan drop-in matching by driver instead of by
  interface name. See ADR-0026's amendment for why the original "no fallback"
  decision was reversed rather than merely revisited.
- **The console pipe name is randomized per boot, and that is a security
  property — do not make it derivable again.** runny *names* the COM0 pipe (the
  compute system document's `ComPorts` entry) but Hyper-V's `vmcompute.exe`
  *creates* it, so runny cannot choose its DACL. Measured on real hardware, that
  DACL is `O:SY G:SY D:(A;;FR;;;WD)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;HA)…` —
  **Everyone gets read** on a guest's serial console, and only SYSTEM/admins/
  Hyper-V admins/the VM account get more. Whoever creates a pipe *name* first
  owns its security descriptor, so the old slot-derived name (stable across
  cycles, derivable from config) could be pre-created by any local user, who
  would then be handed the dial that `fixupNetwork` types the guest's SSH
  credentials into — and whose output `WaitIP` treats as the authoritative lease
  address. `consolePipeName` (`netfixup.go`, untagged so it unit-tests
  off-hardware) now appends 8 random bytes per boot; `hcsMachine.consolePipe`
  carries the value because it can no longer be recomputed. `verifyConsoleOwner`
  (`netfixup_windows.go`) covers the residue by refusing any console whose owner
  is not SYSTEM — the one thing an unprivileged squatter cannot forge, since
  setting an object's owner to SYSTEM needs privilege they don't have. It is
  deliberately narrower than `cmd/runnyctl`'s `isTrustedPipeOwner`, which also
  accepts Administrators: that check authenticates runnyd's own pipe, this one
  authenticates Hyper-V. `slog.Debug` logs the minted name at Boot, since an
  operator attaching to a boot console can no longer derive it.
- **A Windows guest gets no COM port at all; a Linux guest's stays attached for
  the guest's lifetime, and HCS gives no way to change that.** The console's only
  programmatic job is `fixupNetwork`, which `waitIPWindows` never calls, so `Boot`
  omits `ComPorts` entirely for a Windows guest and no pipe is created for it. A
  Linux guest must have one at boot (the fixup is the only way into a guest whose
  networking has not come up), and it cannot be detached afterwards:
  `hcs::System::Modify` on `VirtualMachine/Devices/ComPorts/0` fails with "The
  remote procedure call failed" on every attempt, whether the port is Removed or
  Updated to schema 2.1's documented empty-NamedPipe "disconnected port". Nothing
  in hcsshim modifies a ComPort anywhere -- the type appears only in the schema
  struct -- so there is no upstream precedent to copy and no reason to think a
  different resource path exists. **Do not re-attempt this without new evidence
  from Microsoft**, and note that the attempt is not free: restricted to cycles that
  ran the console fixup, AWAIT_SSH failed 17 of 21 times (81%) on the build that
  made it, against 0 of 25 across the three released builds before it -- dialing
  an address the fixup had just read successfully off the console, and timing
  out. A rejected device
  modify on a running compute system evidently disturbs the guest's networking,
  so this costs guests, not just a warning line. What bounds the exposure instead is the pipe's
  unguessable per-boot name and the SYSTEM-owner check on the dial (above).
- **On the fixup path, `WaitIP` returns the console-observed address, NOT the
  neighbor-table entry.** HNS's `Permanent` neighbor row is a pre-commit written
  before the guest boots, and the guest's own DHCP client can land on a
  *different* `/20` address — returning the pre-commit made `AWAIT_SSH` dial the
  wrong host and destroy-recycle a healthy guest (~1/3 of boots, confirmed on
  hardware). The table can't self-correct: HNS writes the real lease as
  `Permanent` too, so a diverged MAC just shows two `Permanent` rows. `fixupNetwork`
  reads `eth0`'s real address off the console (`parseInetIP`) and `WaitIP` returns
  that; the neighbor table is re-read — **after** the fixup — only to flag the
  divergence (`neighbor-ip-corrected` milestone + a `slog.Warn` listing the stale
  rows via `divergentPermanentIPs`). The re-read must be post-fixup: a fresh
  guest has no pre-commit row for its MAC at grace-elapse, so the stale
  `Permanent` rows the warning reports only materialize once DHCP has settled —
  a pre-fixup snapshot detects nothing and the correction goes silent. The pure
  selectors (`permanentIPs`, `divergentPermanentIPs`, `learnedLeaseIP`) and the
  console parser (`parseInetIP`) live in untagged files (`neighbortable.go`,
  `netfixup.go`) so they unit-test off-hardware.
- **`waitIPLinux`'s grace-period fast path accepts only a LEARNED neighbor row,
  never a `Permanent` one.** A `Permanent` row is HNS's pre-boot pre-commit — a guess
  the guest's DHCP routinely overrides — so returning it within grace would dial
  a stale IP *and* short-circuit before the fixup that would correct it (a live
  landmine for a self-configuring image whose lease diverges from the pre-commit).
  `learnedLeaseIP` (`neighbortable.go`) returns only a `Reachable`/`Stale` row;
  on the validated host HNS surfaces none, so grace always elapses to the fixup,
  which derives the authoritative address from the console. Don't "simplify" this
  back to trusting the neighbor table's `Permanent` entry as the lease — that is
  the exact bug the fixup exists for.
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
- **`hcsMachine.Boot` never attaches a share device for `RunnerShareDir` — it's
  silently a no-op.** Schema 2.1's only Linux-guest-capable share device,
  `Plan9`, is hardware-validated to be rejected outright on a bare (non-LCOW)
  compute system (issue #319): it's paired with LCOW's own guest-side GCS
  bridge protocol, not a device the guest kernel discovers on its own the way
  SCSI/NetworkAdapters are — confirmed by isolating that a generic `Modify`
  (a benign memory-size update) succeeds fine on the same bare compute system,
  so `Plan9` specifically is what's rejected, not hot-modify in general.
  `NeedsRunnerPush() bool` (true on `hcsMachine`, false on `vzMachine`) is how
  the state machine knows to push the runner tarball over SSH instead
  (`internal/guest.Guest.PushRunnerTarball`) — don't "fix" `Boot` to attach a
  `Plan9` device without re-reading this note first.
- **`Boot`'s own per-slot `reapPriorSystem` call and `HCSManager.ReapOrphans`
  (`reap_windows.go`) are two different callers of the same reap, not two
  mechanisms.** An unclean shutdown leaves a slot's compute system (and the
  `vmwp.exe` worker process backing it) running with the slot's clone VHDX
  still open — confirmed on real hardware: `cmd/runnyd`'s cold-start sweep
  (then a single `os.RemoveAll(vmsDir)`) hit exactly that open handle and
  crashed outright, not skip-and-continue, crash-looping the daemon forever
  since nothing ever reaped the orphan in between restarts. `ReapOrphans`
  runs once at startup, before that sweep, over every slot directory found;
  `Boot`'s own call covers the same slot again per-boot as a second line of
  defense. Both go through the `reapOps` seam (`reapPriorSystem`,
  `reapAllSlots`) so the skip/proceed/fail-loud decision logic is
  unit-tested without hardware — only the real HCS/HNS calls (`hcsReapOps`)
  need it. `ReapOrphans` is still best-effort per slot (a genuinely wedged
  compute system that never confirms exit stays un-reaped this restart), so
  `sweepVMsDir` (`cmd/runnyd/main.go`) removes vmsDir's entries one at a
  time rather than the whole tree in one `RemoveAll` — a third line of
  defense so that one still-wedged slot can't crash-loop startup for every
  other slot.
- **Windows guests trust the `Permanent` neighbor row DIRECTLY — this is not a
  regression of the "never trust `Permanent`" rule above, it's guest-OS-
  conditional.** `hcsMachine.WaitIP` dispatches on `guestOS` (set from
  `cfg.OS` at `Boot`) to `waitIPLinux` or `waitIPWindows`. The Linux rule
  exists because the validated Linux image's netplan mismatches hv_netvsc's
  `eth0` naming, so its pre-commit routinely diverges from the real DHCP
  lease. A Windows guest never hits that mismatch — a from-scratch spike
  (`.spike-winboot`, 4 concurrent WS2025 boots, ARP-confirmed) found 0
  divergence — so `waitIPWindows` polls `permanentLeaseIP`
  (`neighbortable.go`) directly: no grace period, no console dial, no
  `fixupNetwork`. If a future Windows image ever needs the fixup too, don't
  "unify" the two paths reflexively — confirm the divergence on real hardware
  first (ADR-0026's Windows-guests amendment), the same discipline that put
  the Linux fixup here in the first place.
