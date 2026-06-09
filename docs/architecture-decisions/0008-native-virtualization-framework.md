# ADR-0008: Full-native Virtualization.framework VM management in tart's format

**Status:** Accepted (2026-06-09)

## Context

runnyd manages ephemeral macOS guest VMs distributed as
[tart](https://github.com/cirruslabs/tart) bundles (`config.json` + `disk.img`
+ `nvram.bin`) pulled from OCI registries (cirruslabs images on ghcr.io).
Three shapes were considered for driving them:

1. Shell out to the tart CLI for everything (the spike-proven incumbent).
2. Hybrid: tart CLI for image pull only; native Virtualization.framework for
   the VM lifecycle.
3. Full native: no tart binary at all.

The original effort estimate flagged "supervising the long-lived `tart run`
child" as the project's hard 20% — tart run must stay alive for the VM's
lifetime, has documented hang reports (tart#831, tart#1162), and emits
unstructured output.

## Decision

**Full native, no tart binary.** runnyd drives Virtualization.framework
in-process via [Code-Hex/vz](https://github.com/Code-Hex/vz), staying fully
compatible with tart's bundle format and OCI distribution so the cirruslabs
image ecosystem keeps working.

Evidence that settled it (initial lean was tart-CLI-behind-an-interface; two
research rounds flipped it):

- **vz is macOS-guest-complete and alive**: `MacOSBootLoader`,
  `MacPlatformConfiguration`, `MacHardwareModel`/`MacMachineIdentifier`/
  `MacAuxiliaryStorage` all exported; complete macOS example in-repo; active
  through Feb 2026.
- **[Cilicon](https://github.com/traderepublic/Cilicon) (MIT) is the reference
  implementation** for exactly this shape; its format layer is ~570 LOC total.
- **Spike-validated 2026-06-09 on ix** (macOS 26 Tahoe): `unix.Clonefile` × 3
  cloned a 120 GB bundle in 957 µs; `vm.Start()` 266 ms; guest IP via
  `/var/db/dhcpd_leases` at 8.3 s; SSH banner at 11.2 s. Pure Go, CLT only.
- **Strategic**: Cirrus Labs joined OpenAI 2026-04; depending on the binary at
  runtime now carries stewardship risk the format (mitigated by the Cilicon
  reference + digest pinning) does not.

Mechanics: clone = `clonefile()` over three files; guest IP = dhcpd_leases
parsing (octets are zero-stripped — normalize); OCI pull = own client
(tart's layout is non-standard, LZ4 layers, bearer/basic auth); fresh
`MacMachineIdentifier` + fresh MAC per clone.

## Consequences

- Deletes the child-supervision problem entirely; every VM operation is an
  in-process call the FSM deadline-bounds directly.
- In-process VM lifetime (daemon dies → VMs die) aligns with crash-only
  ephemeral runners; restart is a cold start (ADR-0004).
- cgo + Virtualization.framework: runnyd must be codesigned with the
  `com.apple.security.virtualization` entitlement (ad-hoc OK locally); daemon
  builds are macOS-only.
- We own format-tracking if tart's bundle/OCI format churns. `diskFormat:
  asif` (macOS 26 tart option) is rejected with a clear error until verified.

## Rejected alternatives

- **tart CLI for everything**: spike-proven and least code, but keeps the
  hard-20% child supervision, unstructured output parsing, and a runtime
  binary dependency with uncertain stewardship.
- **Hybrid (tart pull + native lifecycle)**: defensible v1, but the pull
  client is ~250 LOC against a working MIT reference — not worth carrying the
  binary dependency to avoid.
