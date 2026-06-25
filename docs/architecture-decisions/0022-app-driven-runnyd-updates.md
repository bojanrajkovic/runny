# ADR-0022: App-driven runnyd updates with a bundled-binary config-compat gate

**Status:** Accepted (2026-06-24)

## Context

A runnyd update is *delivered* but not *applied* until something drains the
running daemon and respawns it onto the new binary. Two gaps make that dangerous
or absent today.

**A schema-incompatible upgrade crash-loops silently.** The per-user agent's
"Update" issues the Reload RPC; the reload preflight runs inside the *running*
daemon and validates the on-disk config against *that* (old) binary's schema
([ADR-0014](0014-config-reload-drain-and-respawn.md),
[ADR-0017](0017-confirming-reload-convergence.md)). It cannot answer the question
an upgrade actually poses — does the *new* binary accept the *current* config?
When a release changes the config schema (strict parse rejects an unknown or
renamed key; a validation rule tightens), the drain proceeds, launchd cold-starts
the new binary, and it fails startup and crash-loops under KeepAlive — the fleet
is down with no in-place repair. A silent-failure of exactly the kind this
project exists to eliminate.

**The system daemon has no update path at all.** The post-upgrade affordance is
offered only for the app's own per-user LaunchAgent; an installed system daemon
([ADR-0020](0020-headless-system-daemon.md)) gets only the skew banner
([ADR-0018](0018-bundled-app-distribution.md)). On a headless host there may be no
GUI watching at all.

## Decision

Gate every daemon update on the **new binary validating the in-place config**,
auto-apply it where that is free, and give the system daemon a real, surfaced,
gated update path.

- **A bundled-binary config-compat gate is the shared substrate.** Before an
  update commits, the *new* `runnyd` validates the *in-place* config and returns a
  machine-readable verdict. This is the one mechanism every consumer shares; only
  the new binary can answer the new-schema question.

- **`runnyd -test-config <path>` runs local checks only and emits JSON.** It loads
  the config and runs the deterministic, local startup checks — strict parse,
  `validate()`, the macOS guest-cap, the runner-namespace — plus the
  soft-validations below, and prints `{status: ok|warn|error, errors, warnings}`.
  It runs **no** network checks: upgrade-readiness is a question about
  config-schema compatibility, not live GitHub/registry/disk health, and coupling
  the two would let a transient API blip refuse a valid upgrade. This is distinct
  from `-doctor`, which runs the full network suite for operational diagnosis.

- **The verdict is three-way: OK / Warn / Error.** OK applies the update; Warn
  surfaces the warnings and drops to a manual confirmation; Error blocks and names
  the incompatibility. The Warn tier is backed by a non-fatal warnings channel in
  the config loader, seeded with two local soft-validations: **resource
  over-allocation** (summed across concurrent slots, `count × cpu_cores` exceeds
  the host's logical cores or `count × ram_gb` exceeds its physical RAM — per-guest
  fit is already validated at boot, but the aggregate overcommit is not) and
  **deadline-too-short** (a `deadlines.*` below a conservative floor). Warnings
  never block a load; they inform the verdict.

- **The per-user agent auto-applies on OK, by default.** A default-on setting;
  when the app is the newer bundle and the gate returns OK, it applies the
  drain-gated reload without waiting for a click. That reload is free — the agent's
  `BundleProgram` already points at the new binary, so a drain-gated respawn
  cold-starts onto it with no privilege. Warn drops to a manual CTA; Error blocks.

- **The system daemon is one daemon with two delivery channels; the binary-path
  split stands.** A Homebrew install execs the brew opt-symlink (kept current by
  `brew upgrade`); an app-brokered install execs a staged copy under
  `/usr/local/libexec/runny` ([ADR-0020](0020-headless-system-daemon.md)). The
  daemon-side update is identical — the config-compat gate, then a drain-gated
  reload — and only binary *delivery* differs.

- **The app-brokered re-stage replaces the staged binary with an atomic rename.**
  It writes the new binary to a temp path in the staging dir, then `rename(2)`s
  over the live path; the running daemon keeps the old inode until the drain-gated
  reload exits it, and launchd cold-starts the new one. A direct copy-over is
  unsafe: macOS does not raise `ETXTBSY` on writing a running executable — it
  truncates the live inode in place, which corrupts or code-sign-kills the running
  process on its next page fault. (The existing installer's `rm -f` before `cp` is
  what makes the *install* path safe — it unlinks first, yielding a fresh inode;
  the rename additionally closes the absent/partial window a KeepAlive restart
  could exec into.) The re-stage is privileged and runs only after the gate returns
  OK, so the admin prompt is never spent on a doomed upgrade.

- **The headless path is operator-driven; the daemon never self-upgrades.**
  `runnyctl upgrade-daemon` runs the on-disk (new) binary's `-test-config` against
  the in-place config and, on OK, issues the drain-gated reload; brew already
  delivered the binary, so there is no re-stage. A daemon that watched its own
  binary and respawned itself would add a self-triggered restart to a crash-only
  daemon whose entire model is that restarts come from launchd, not itself
  ([ADR-0004](0004-crash-only-state-machine.md)).

- **Skew is surfaced on non-GUI channels.** `runnyctl doctor` and the CLI skew
  warning name "a newer runnyd is available — run `runnyctl upgrade-daemon`" when
  the daemon lags the on-disk binary, and the daemon logs the same. A headless host
  has no GUI to nag, so the surfacing lives where the operator already looks.

## Rejected alternatives

- **Trust the running daemon's reload preflight.** It validates against the old
  binary's schema and is structurally blind to the new one — the exact gap this
  closes.

- **Reuse the full `-doctor` suite as the gate.** Its network checks make the gate
  slow and flaky and couple upgrade-readiness to momentary GitHub/registry/disk
  state; a blip would refuse a valid upgrade.

- **OK/Error only, no Warn tier.** Loses the "upgrade works but your config is
  suspect" signal; the soft-validations are cheap, local, and catch real operator
  footguns.

- **Button-only updates, no auto-apply.** The per-user reload is free and safe on
  OK; defaulting to manual leaves the silent-skew window open longer for no
  benefit. The setting still lets an operator pin a version.

- **Unify the system daemon to an always-staged libexec copy.** Reverses the
  opt-symlink choice of [ADR-0020](0020-headless-system-daemon.md) and adds a
  privileged re-stage to the brew path to guard only an operator-error case
  (`brew uninstall` without `uninstall-daemon`). The split costs nothing the gate
  does not already share.

- **Symlink a launchd-managed binary into the app bundle.** The one pattern the
  ecosystem uniformly avoids — Tailscale's System Extension, Docker's and
  OrbStack's privileged helpers, and Apple's SMJobBless/SMAppService all use a
  staged copy or an OS-managed bundle extension, never a symlink into a deletable
  bundle. A drag-to-Trash means "remove the GUI," not "uninstall runny," and has no
  hook, so it would dangle the link and kill a headless fleet on the next restart.
  The staged copy is the deliberate decoupling.

- **`cp`-over the running staged binary.** Silently mutates a live binary in place
  on macOS (no `ETXTBSY` guard) — a corruption/kill hazard, and non-atomic besides.

- **A daemon that self-upgrades.** Adds a self-triggered restart loop to a daemon
  whose recovery model is launchd-owned cold starts.

## Consequences

- Supersedes the "a system daemon is offered only the generic skew banner, not an
  update" stance of [ADR-0018](0018-bundled-app-distribution.md) (amended there):
  the system daemon now has a real, config-gated update. The binary-path decisions
  of [ADR-0020](0020-headless-system-daemon.md) stand unchanged.

- The `-test-config` JSON is a cross-language contract: the Swift app and the Go
  `runnyctl` both exec the new `runnyd` and parse the same verdict, so its schema
  is stable surface versioned with the daemon.

- The config loader grows a non-fatal warnings channel; the existing reload
  preflight inherits it (warnings flow through, with no change to what the reload
  accepts or refuses).

- The app gains a system-daemon update affordance — today the update verdict is
  `none` for a system daemon — gated on the ownership verdict
  ([ADR-0019](0019-daemon-ownership-detection.md)) and the config-compat gate,
  brokering the re-stage and reload.

- `runnyctl` gains an `upgrade-daemon` subcommand and a doctor/skew line naming it.

- The deprecation-warning channel is reserved: the Warn tier ships with the two
  soft-validations above, and a real schema deprecation populates it later.

- Implementation is a four-epic rollout tracked on the project board — the
  config-compat substrate, the per-user auto-apply, the app-brokered re-stage, and
  the headless CLI path — each a small vertical slice over this design.
