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
`<pool>-<n>`; runner names are `<instance-prefix>-<slot>-<cycle8>`.

The instance prefix is **derived, not configured**: `<slug(hostname)>-<rand8>`,
generated once and persisted in `~/.runny/instance-id`. It is the daemon's
ownership namespace — the startup sweep deletes offline registrations by
matching it — so it is deliberately not a config knob: a mistyped prefix would
orphan runners beyond the sweep's reach or collide with another host's, and it
is persisted (not regenerated per process) so a crash-restart keeps the same
namespace. The host slug makes runners human-identifiable; `rand8` disambiguates
same-hostname hosts and anchors stability if the hostname later changes.
*(Revised 2026-06-10: replaced the configurable `name_prefix`, which put a
sweep-critical identifier in fragile operator hands.)*

Consequences through the stack:

- **github**: a `Target` (org xor owner/repo) selects the endpoint family
  (`/orgs/{org}/...` vs `/repos/{o}/{r}/...`) and the permission the doctor
  asserts on a minted token: `administration: write` for repos,
  `organization_self_hosted_runners: write` for orgs. App credentials are
  **per-pool**, not shared: different targets are different App installations
  with different keys (a personal repo and an org are not the same App), so
  each pool carries its own `github` block. One client per distinct
  (App, target). *(Revised 2026-06-10: the original "credentials are shared"
  assumption broke the first real mixed fleet — a personal-repo test pool and
  the loupe-app org pool need different Apps.)*
- **vm**: `Boot` dispatches on the bundle's `os` — the existing
  Mac platform path for darwin, an EFI path (`VZEFIBootLoader` + EFI
  variable store from `nvram.bin`, generic platform) for linux. Linux
  bundles carry no hardwareModel/ecid; validation is per-OS. Guest CPU/RAM
  default to the image's baked `config.json` values; a pool may override
  them (`cpu_cores`, `ram_gb`) — a request below the bundle's recorded
  minimum is rejected, not clamped.
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
