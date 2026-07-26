# ADR-0026: Hyper-V VM backend on bare HCS, guest networking trusted to cloud-init

**Status:** Accepted (2026-07-22)

**Amended:** 2026-07-24 — Windows guest support added. `HCSManager.Boot` now
accepts `cfg.OS == "windows"` alongside `"linux"`, selecting the Windows
Secure Boot template GUID (`1734c6e8-3154-4dda-ba5f-a874cc483422` vs the
Linux-shim anchor above) — everything else in the compute-system document
(schema 2.1, `HvSocket`, SCSI-attached VHDX, `ComPorts`, `NetworkAdapters`)
is unchanged and guest-OS-agnostic. `WaitIP` now dispatches on guest OS
(`waitIPLinux`/`waitIPWindows`, `internal/vm/hcs_windows.go`): a Windows
guest trusts HNS's `Permanent` pre-commit row **directly**, the opposite of
this ADR's own Q1 finding for Linux. That is not a contradiction — Q1's
"never trust Permanent" is a consequence of the specific Linux/hv_netvsc
netplan mismatch below, not a property of HNS's pre-commit itself. A
from-scratch spike (`.spike-winboot`, 4 concurrent WS2025 boots,
ARP-confirmed) found 0 divergence between the pre-commit and the guest's
real lease for Windows, because Windows never hits that mismatch. So
`WaitIP` needs no grace period and no console fixup on the Windows path;
`permanentLeaseIP` (`internal/vm/neighbortable.go`) is the pure selector,
mirroring `learnedLeaseIP`'s determinism for the (unobserved, but
theoretically possible) multiple-Permanent-row case. `Bundle.LoadConfig`
accepts both `windows/amd64` and `windows/arm64` — arch validation stays
`checkHostArch`'s job, not a per-OS hardcode (Context Q4's existing
reasoning, now applied to Windows guests too, not just Linux). See the
Decision and Consequences sections below, updated in place.

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

A `Permanent` row is never trusted as the guest's address. It is HNS's pre-boot
pre-commit, and the guest's own DHCP client (HNS runs a DHCP server over the
`/20`) routinely lands on a different address — and the neighbor table gives no
way to tell the two apart, since HNS writes the real lease as `Permanent` too,
not as a dynamically-learned (`Reachable`/`Stale`) row, so a diverged MAC simply
carries two `Permanent` rows at different IPs. So the neighbor table is the IP
source only within the grace period, and only for a *learned* row — an actual
ARP resolution proving a self-configuring guest is reachable. A guest that needs
the fixup surfaces no learned row, grace elapses, and its real address is read
off the console during the fixup (below), where the guest reports it directly.

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
(`{darwin+arm64, linux+arm64, linux+amd64, windows+arm64, windows+amd64}`); each
platform's own `Boot` adds the host-capability rejection against its own
`runtime.GOARCH`. Windows guests follow the exact same rule as Linux guests here —
both arches accepted by `LoadConfig`, the real host-capability gate left entirely to
`checkHostArch` — not the `windows ⇒ amd64` hardcoding this section opens by
rejecting.

## Decision

Bare HCS compute systems (`internal/winhcs/hcs`), not classic vmms-managed VMs.
Schema 2.1, `HvSocket` populated, `SecureBoot` applied via a per-guest-OS
template (below), `Chipset.Uefi.BootThis` omitted (UEFI auto-discovers the ESP on
the SCSI-attached VHDX — the documented `ScsiDrive`/`File` boot-entry `DeviceType`
values are unexercised even in Microsoft's own code and do not work). `WaitIP`
polls `GetIpNetTable2` via a small hand-written binding
(`internal/vm/neighbortable_windows.go`), since `x/sys/windows` doesn't wrap this
corner of `iphlpapi`; the pure row-selection logic lives in
`internal/vm/neighbortable.go` so it's unit-tested off-hardware.

### Linux guests

`WaitIP` gives the guest a bounded grace period (`waitIPGracePeriod`) to bring
up networking on its own — the original assumption below, kept as the first
thing tried. Within grace it accepts only a *learned* (`Reachable`/`Stale`)
neighbor row — an ARP resolution proving the guest is actually reachable — never
HNS's `Permanent` pre-commit (Context Q1). If the grace period elapses with no
learned row, it falls back once to a console-driven fixup
(`internal/vm/netfixup_windows.go`):
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
and only for a learned row — a self-configuring guest that has actually resolved,
never a bare `Permanent` pre-commit.

### Windows guests

The compute-system document is otherwise identical; the guest OS only changes
one field, `Chipset.Uefi.SecureBootTemplateId` (`HCSManager.Boot`/
`hcs_windows.go`): the Windows Secure Boot template GUID
`1734c6e8-3154-4dda-ba5f-a874cc483422` in place of the Linux-shim anchor above.
`WaitIP` also branches (`waitIPLinux`/`waitIPWindows`): a Windows guest never
hits the Linux path's hv_netvsc/netplan naming mismatch, so its `Permanent`
pre-commit row genuinely is the guest's real lease — a from-scratch spike
(`.spike-winboot`, 4 concurrent WS2025 boots, ARP-confirmed) found 0 divergence.
`waitIPWindows` therefore trusts `permanentLeaseIP` directly: no grace period,
no learned-row distinction, no console fixup. This is a guest-OS-conditional
trust decision, not a reopening of Q1's "never trust `Permanent`" finding —
that finding is specific to the Linux image's netplan behavior, which the
Windows path never exercises. The spike validated this on windows/amd64 only;
windows/arm64 is accepted by `LoadConfig` on the same "real but currently
unvalidated target" footing as linux/arm64 (Context Q4) — nothing about the
mechanism (HNS's own DHCP pre-commit) is arch-specific, but re-confirm on
arm64 hardware before leaning on that extrapolation for something load-bearing.

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
  netplan actually self-adapts never triggers it — it resolves to a *learned*
  neighbor row within the grace period and `WaitIP` returns before the fallback
  ever runs.
- On the fallback path the console read, not the neighbor table, is `WaitIP`'s
  IP source: HNS's `Permanent` pre-commit can diverge from the guest's real DHCP
  lease, and since HNS writes the real lease as `Permanent` too, no neighbor-table
  read can recover it. The `neighbor-ip-corrected` milestone plus the corrected
  `vm.ip` make the divergence visible in a trace.
- Guest arch validation is `runtime.GOARCH`-relative, not hardcoded to amd64 — an
  arm64 Windows host booting `linux/arm64` guests needs no code change here.
- A Windows guest's `WaitIP` has no console-fallback failure mode to reason
  about — the entire fixup/divergence-detection machinery above is Linux-only.
  A Windows guest that never gets a `Permanent` row at all (endpoint creation
  failed, or the guest never DHCPs) times out the same way any other stalled
  `WaitIP` does, with no special-cased error message the Linux path's
  `SSHUser`/`SSHPassword`-missing check gets.

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

**Amended:** 2026-07-26 — the console pipe is authenticated, and its name is
unguessable. The fixup above dials a pipe runny only *names*; Hyper-V's
`vmcompute.exe` creates it, so runny does not choose its DACL. Reading that DACL
off a live guest showed `O:SY G:SY D:(A;;FR;;;WD)(A;;FA;;;SY)(A;;FA;;;BA)
(A;;FA;;;HA)…` — Everyone can read a guest's serial console, and only
SYSTEM/administrators/Hyper-V administrators/the VM's own account get more.
Since whoever creates a pipe name first owns its security descriptor, the
original slot-derived name — stable across cycles and derivable from config —
could be pre-created by any local user, who would then receive the dial this
fixup types the guest's SSH credentials into, and whose console output `WaitIP`
treats as the authoritative lease address. Two changes close that: the name now
carries 8 random bytes minted per boot, so it cannot be pre-created; and the
dial refuses any console whose owner is not SYSTEM, which is the part a squatter
cannot forge. Neither changes what the fixup does or when it runs.
