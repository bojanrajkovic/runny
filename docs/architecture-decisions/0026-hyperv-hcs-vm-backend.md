# ADR-0026: Hyper-V VM backend on bare HCS, guest networking trusted to cloud-init

**Status:** Accepted (2026-07-22)

**Amended:** 2026-07-23 — `WaitIP`'s IP *source* is corrected. The host neighbor
table's `Permanent` row is HNS's pre-commit, written at endpoint-attach before
the guest boots, and the guest's own DHCP client can land on a **different**
address in the `/20` — so returning the pre-commit made a healthy guest's
`AWAIT_SSH` dial the wrong host and destroy-recycle. Confirmed on real hardware
(roughly a third of overnight boots) and characterized directly: HNS surfaces
*every* row as `Permanent`, including the guest's real lease, so a diverged MAC
carries two `Permanent` rows at different IPs and no learned-state row ever
distinguishes the real one. `WaitIP` therefore returns the address the guest
itself reports on the console once the fixup has run — authoritative by
construction — and reads the neighbor table only to record the divergence. See
the updated Decision and Consequences.

**Amended:** 2026-07-22 — the "no console-based networking fallback" half of
this decision is reversed. The rejected alternative's own reopening
condition ("if `WaitIP` timeouts on a real image ever demonstrate the need")
fired immediately: the currently-validated image's baked netplan matches
`en*`, hv_netvsc always names the NIC `eth0`, and without a live fixup `eth0`
never comes up at all — confirmed on real hardware from a from-scratch
differencing-disk boot with zero prior state, and again on a plain classic
Hyper-V VM, so it isn't an artifact of a dirtied image or of bare HCS
specifically. `WaitIP` now gives the guest a bounded grace period to
self-configure (preserving this ADR's original assumption for whatever
image might actually satisfy it), then falls back once to a console-driven
netplan drop-in (`internal/vm/netfixup_windows.go`) if the grace period
elapses with no neighbor-table entry. See the Decision and Consequences
sections below, updated in place rather than left describing a design the
shipped code no longer matches.

## Context

runnyd's Windows host path needs the same job ADR-0008 solved for darwin: drive an
ephemeral Linux guest VM in-process, from a tart-format bundle, with no long-lived
external binary to supervise. The vendored HCS binding (`internal/winhcs`, trimmed from
`Microsoft/hcsshim`) and the VHDX differencing-clone primitive (`internal/vhdx`) already
existed as prerequisites; this decision is about how `internal/vm/hcs_windows.go` uses
them, mirroring `vz_darwin.go`'s `Manager`/`Machine` shape.

Four questions had real forks, each validated against a live Hyper-V host
(Windows 10 Pro build 19045) before this decision:

**1. How does `WaitIP` learn the guest's address?** HNS pre-commits a guest's MAC→IP
binding into the host's IP neighbor table (`GetIpNetTable2`) at endpoint attach —
state `Permanent`, present within seconds of `Start`, well before the guest OS could
have booted. `hcn.GetEndpointByID`'s `IpConfigurations` was tried first and rejected:
it never reflects the lease for a vmms-attached endpoint (spike-validated for issue
#307), and — checked again for this decision on a bare HCS-created endpoint — reads
inconsistently, empty at first and populated later, unlike the neighbor table's
immediate, stable read. KVP (`Get-VMNetworkAdapter` IP addresses) is unreachable for a
bare compute system: it bypasses vmms entirely, which owns KVP.

The pre-commit is only *usable* as the guest's address when the guest's own DHCP
client happens to take it. It often does not: HNS runs its own DHCP server over
the `/20`, and the lease the guest actually gets can be a different address than
the pre-committed one. The neighbor table gives no way to tell the two apart —
HNS writes the real lease as `Permanent` too, not as a dynamically-learned
(`Reachable`/`Stale`) row, so a diverged MAC simply carries two `Permanent` rows
at different IPs. The guest's real address is therefore read off the console
during the network fixup (below), where the guest reports it directly; the
neighbor table stays the source only for a guest that self-configures within the
grace period, where the single pre-commit row is correct by construction.

**2. Does the guest need live console interaction to get networking up?** The
concern (hv_netvsc naming the NIC `eth0` while a stock image's netplan might match
`en*`) is real in general, but empirically false for the validated image
(`ghcr.io/cirruslabs/ubuntu-runner-amd64`): its `/etc/netplan/50-cloud-init.yaml` is
cloud-init's own dynamically-generated fallback config, which DHCP-configures
whatever NIC actually exists by name rather than matching a static installer-time
pattern — confirmed reachable over TCP:22 with zero console interaction, end to end,
using the production `Boot`/`WaitIP`/`Stop` path directly. Verified this wasn't an
artifact of a hand-patched test image: the pulled `disk.img`'s 75 layers all matched
their manifest-declared content digests exactly, byte for byte.

**3. When does the differencing-VHDX clone happen?** Not inside `Boot` — inside the
CLONE state's `Cloner` (`internal/tart.CloneVHDX`), same as darwin's clonefile-based
`Clone`. The one-time raw-to-VHDX *conversion* (`internal/vhdx.Convert`) runs inside
`imagePuller.run()` — the single actor every concurrent slot's `Ensure` subscribes to
— so concurrent slots pulling the same image never race the conversion. `disk.img` is
removed once `disk.vhdx` exists; `Bundle.Verify()` accepts either, so this costs
nothing on the next `Ensure`'s cache-hit check.

**4. What decides whether a guest's declared arch is bootable?** Not a hardcoded
per-OS arch (`windows` ⇒ `amd64`): neither Hyper-V nor Virtualization.framework
cross-emulates architectures (Rosetta 2 translates *userspace binaries* inside an
already-booted arm64 Linux guest; it does not let VZ boot an x86_64 kernel), so the
real constraint is host-relative — a guest's arch must equal the process's own
`runtime.GOARCH`. `Bundle.LoadConfig` stays a portable, host-independent shape check
(`{darwin+arm64, linux+arm64, linux+amd64}`); each platform's own `Boot` adds the
host-capability rejection against its own `runtime.GOARCH`.

## Decision

Bare HCS compute systems (`internal/winhcs/hcs`), not classic vmms-managed VMs.
Schema 2.1, `HvSocket` populated, `SecureBoot` applied via the Microsoft UEFI CA
template, `Chipset.Uefi.BootThis` omitted (UEFI auto-discovers the ESP on the
SCSI-attached VHDX — the documented `ScsiDrive`/`File` boot-entry `DeviceType`
values are unexercised even in Microsoft's own code and do not work). `WaitIP`
polls `GetIpNetTable2` for a matching entry via a small hand-written binding
(`internal/vm/neighbortable_windows.go`), since `x/sys/windows` doesn't wrap this
corner of `iphlpapi`; the pure row-selection logic lives in
`internal/vm/neighbortable.go` so it's unit-tested off-hardware.

`WaitIP` gives the guest a bounded grace period (`waitIPGracePeriod`) to bring
up networking on its own — the original assumption below, kept as the first
thing tried. If the grace period elapses with no neighbor-table entry, it
falls back once to a console-driven fixup (`internal/vm/netfixup_windows.go`):
dial the HCS console pipe, log in, and apply a netplan drop-in matching by
driver (`match: driver: hv_netvsc`) rather than by interface name, then read
`eth0`'s address back off the console. This is the demonstrated case for the
currently-validated image (`ghcr.io/cirruslabs/ubuntu-runner-amd64`): its
baked netplan matches interface names `en*`, which hv_netvsc's always-`eth0`
naming never satisfies, so `eth0` sits down and DHCP never even starts
without it.

That console-read address — the one the guest actually holds — is what `WaitIP`
returns on the fixup path, **not** the neighbor-table pre-commit, which can name
a stale, unreachable address (Context Q1). To make the correction visible,
`WaitIP` re-reads the neighbor table *after* the fixup and, if any `Permanent`
row for the MAC now differs from the console lease, emits a
`neighbor-ip-corrected` trace milestone and logs the stale rows. The re-read has
to be post-fixup: a fresh guest has no pre-commit row for its MAC when the grace
period elapses, so the stale rows only materialize once DHCP has settled — a
pre-fixup snapshot would detect nothing and the correction would go silent. The
neighbor-table read remains the *IP source* only on the grace-period happy path,
where a self-configuring guest is reachable at the pre-commit by construction.

## Consequences

- HCS compute systems are invisible to `Get-VM`/Hyper-V Manager — the `vmwp.exe`
  worker process is the operator-visible artifact on this host.
- The neighbor-table entry HNS programs does **not** clear itself when the endpoint
  is deleted (empirically confirmed: it outlived `Terminate`, endpoint `Delete`, and
  30s of polling after both, across three separate runs) — `hcsMachine.Stop` scrubs
  it explicitly once the guest is confirmed stopped, or it would accumulate one stale
  row per boot cycle for the daemon's whole lifetime.
- A console-based networking fallback exists and is mandatory in practice for the
  currently-validated image (see above). A future or different guest image whose
  netplan actually self-adapts never triggers it — it gets its neighbor-table entry
  within the grace period and `WaitIP` returns before the fallback ever runs.
- On the fallback path the console read, not the neighbor table, is `WaitIP`'s
  IP source: HNS's `Permanent` pre-commit can diverge from the guest's real DHCP
  lease, and since HNS writes the real lease as `Permanent` too, no neighbor-table
  read can recover it. The `neighbor-ip-corrected` milestone plus the corrected
  `vm.ip` make the divergence visible in a trace.
- Guest arch validation is `runtime.GOARCH`-relative, not hardcoded to amd64 — an
  arm64 Windows host booting `linux/arm64` guests needs no code change here.

## Rejected alternatives

- **`hcn.GetEndpointByID` / KVP for `WaitIP`**: both tried and rejected per the
  Context section above — neither reflects a bare compute system's lease reliably.
- **Hardcoding guest arch to amd64 per OS**: rejected once it became clear an arm64
  Windows host is a real (if currently unvalidated) Hyper-V target this project
  shouldn't need a second code path for.

## Superseded rejection: the console-interaction DHCP fixup

Originally rejected here "in favor of trusting the validated image's actual
behavior," with an explicit reopening condition: "the fixup can be added
later if `WaitIP` timeouts on a real image ever demonstrate the need." That
condition fired on the very first real-hardware validation — trusting the
image's behavior was the wrong call, not a defensible one that later
changed. The fixup is now built, gated behind `WaitIP`'s grace period so it
still costs nothing for an image that doesn't need it (see Decision above).
