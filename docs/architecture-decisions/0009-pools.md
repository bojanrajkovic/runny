# ADR-0009: Pools — mixed-OS guests against mixed org/repo targets

**Status:** Accepted (2026-06-09)

## Context

The first working configuration was one image, one repo-scoped registration
target, N identical macOS slots. Two needs broke that shape at once:

1. **Linux guests.** Linux runners are cheap to host next to the macOS fleet
   (tart-format ubuntu images exist, and Virtualization.framework's
   2-concurrent-guest cap applies to *macOS* guests only), and the homelab
   has Linux CI demand.
2. **Org-level registration.** The production fleet (sand) serves the
   loupe-app *organization*, while ad-hoc projects want repo-scoped runners.
   A daemon that can't mix both can't replace sand.

## Decision

The fleet is a list of **pools**. Each pool declares its guest `os`
(darwin | linux), `image`, `count`, registration `target` (an org, or an
owner/repo pair), `labels`, and optional overrides. Slots are named
`<pool>-<n>`; runner names stay `<prefix>-<slot>-<cycle8>`.

Consequences through the stack:

- **github**: a `Target` (org xor owner/repo) selects the endpoint family
  (`/orgs/{org}/...` vs `/repos/{o}/{r}/...`) and the permission the doctor
  asserts on a minted token: `administration: write` for repos,
  `organization_self_hosted_runners: write` for orgs. One client per
  distinct target; the App credentials are shared.
- **vm**: `Boot` dispatches on the bundle's `os` — the existing
  Mac platform path for darwin, an EFI path (`VZEFIBootLoader` + EFI
  variable store from `nvram.bin`, generic platform) for linux. Linux
  bundles carry no hardwareModel/ecid; validation is per-OS.
- **guest**: the provision script is per-OS (mount semantics, runner
  tarball flavor, `installdependencies.sh` on linux).
- **images**: the runner-tarball cache holds one tarball per guest OS,
  each resolved from the target's `/actions/runners/downloads`.
- **doctor**: the 2-guest cap check sums *darwin* pool counts only.

## Rejected alternatives

- **Separate daemons per target/OS**: N daemons fighting over the macOS
  guest cap with no global view — the cap is host-wide, so its enforcement
  must be too.
- **Org-only (sand's effective shape)**: cannot serve repo-scoped projects
  without granting the App org-wide runner rights everywhere.
- **OS inferred from the image at pull time**: the cap check and tarball
  priming need the OS *before* anything is pulled; an explicit `os` field
  costs one line and validates against the bundle at boot.
