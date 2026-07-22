# ADR-0026: Hyper-V VM backend on bare HCS, guest networking trusted to cloud-init

**Status:** Accepted (2026-07-22)

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
polls `GetIpNetTable2` for a `Permanent` entry matching the guest's MAC — a small
hand-written binding (`internal/vm/neighbortable_windows.go`), since
`x/sys/windows` doesn't wrap this corner of `iphlpapi`. No console interaction: the
guest is trusted to self-configure networking.

## Consequences

- HCS compute systems are invisible to `Get-VM`/Hyper-V Manager — the `vmwp.exe`
  worker process is the operator-visible artifact on this host.
- The neighbor-table entry HNS programs does **not** clear itself when the endpoint
  is deleted (empirically confirmed: it outlived `Terminate`, endpoint `Delete`, and
  30s of polling after both, across three separate runs) — `hcsMachine.Stop` scrubs
  it explicitly once the guest is confirmed stopped, or it would accumulate one stale
  row per boot cycle for the daemon's whole lifetime.
- No console-based networking fallback exists. If a future or different guest image
  ships a static, non-self-adapting netplan, `WaitIP` simply times out — that's the
  signal to revisit this decision, not something defended against speculatively now.
- Guest arch validation is `runtime.GOARCH`-relative, not hardcoded to amd64 — an
  arm64 Windows host booting `linux/arm64` guests needs no code change here.

## Rejected alternatives

- **`hcn.GetEndpointByID` / KVP for `WaitIP`**: both tried and rejected per the
  Context section above — neither reflects a bare compute system's lease reliably.
- **Building the console-interaction DHCP fixup anyway, defensively**: rejected in
  favor of trusting the validated image's actual behavior; the fixup can be added
  later if `WaitIP` timeouts on a real image ever demonstrate the need.
- **Hardcoding guest arch to amd64 per OS**: rejected once it became clear an arm64
  Windows host is a real (if currently unvalidated) Hyper-V target this project
  shouldn't need a second code path for.
