# ADR-0023: The app is non-privileged — system + root + CLI install are runnyctl's

**Status:** Accepted (2026-06-26)

Enacts the [privilege-boundary](../architecture/privilege-boundary.md) principle
and supersedes, in part, the app-privilege portions of
[ADR-0018](0018-bundled-app-distribution.md),
[ADR-0020](0020-headless-system-daemon.md), and
[ADR-0022](0022-app-driven-runnyd-updates.md) — see Consequences for exactly which.

## Context

The app accreted three privileged paths, each behind a `with administrator
privileges` prompt brokered through one shared `osascript` runner:

1. **Install/remove the `system/` LaunchDaemon** — stage the bundled binaries to
   `/usr/local/libexec/runny` and run `runnyctl install-daemon` as root
   ([ADR-0020](0020-headless-system-daemon.md)'s app path).
2. **Re-stage the system daemon's binary** for an update — an atomic root
   tmp-write→`rename(2)` ([ADR-0022](0022-app-driven-runnyd-updates.md)).
3. **Symlink `runnyctl` into `/usr/local/bin`** when that directory needs admin
   ([ADR-0018](0018-bundled-app-distribution.md)'s CLI vending).

All three exist only to drive a one-time `sudo` from a GUI, and all three carry
real complexity: a single audited AppleScript/shell escaping path, a process
runner that can cancel a standing prompt, translocation fail-closed guards, and
confirm-from-disk-not-exit-code recovery — the `PrivilegedBroker` plus two
installer state machines and their pure surfaces.

The audience math does not support that complexity. A **headless fleet host** —
the only place a `system/` LaunchDaemon is the right shape — has no desktop
session to click an admin prompt, so it installs and updates the daemon with
`runnyctl install-daemon` / `upgrade-daemon` directly; the app's brokered path is
a worse way to reach the same on-disk result. A **desktop user** needs only the
per-user LaunchAgent (no prompt) and reaches `runnyctl` through the Homebrew cask
or the bundle. So every privileged path in the app is either redundant with
`runnyctl` (system daemon, CLI install) or serves no one (a GUI brokering a
`sudo` for a headless deployment).

## Decision

**The app owns only the per-user `gui/` launchd domain and raises no
administrator prompt, ever.** `system/`, root, and installing `runnyctl` to a
system path are `runnyctl`'s domain. The principle and the full may/must-not list
live in [privilege-boundary.md](../architecture/privilege-boundary.md); the
enacting changes:

- **Remove app-brokered system-daemon management** — install, uninstall, and the
  update re-stage. The system daemon is installed, removed, and updated by
  `runnyctl install-daemon` / `uninstall-daemon` / `upgrade-daemon`, run by the
  operator.
- **Remove app CLI-symlink install.** `runnyctl` reaches PATH via the Homebrew
  cask or by running it from inside the bundle; the app stops vending it.
- **Delete the privileged broker** (`PrivilegedBroker`) and both installer models
  (`SystemDaemonInstaller`, `CLIInstaller`/`CLIInstallPlan`) with the Settings
  rows and the menu-bar CLI nudge they backed.
- **Keep read-only observation of a system daemon.** The unprivileged `launchctl
  print system/<label>` ownership probe stays: the app still observes a
  system-managed daemon over the shared socket and shows an observer banner — now
  pointing the operator at `runnyctl uninstall-daemon` rather than a deleted
  Settings section. The probe needs no privilege, so the boundary does not touch
  it.
- **Keep the entire non-privileged per-user app.** The `SMAppService` per-user
  agent (register/start/uninstall/repair — all `gui/`-domain, no prompt),
  status/doctor/logs/timeline, the Local Network grant, skew, and the per-user
  update affordance (a drain-gated reload *is* the upgrade) are unchanged.

## Rejected alternatives

- **Keep app-brokered system-daemon management.** It duplicates `runnyctl
  install-daemon` for an audience that can't use a GUI prompt anyway, and it is
  the single largest privileged surface in the app. Hardening it (the broker, the
  re-stage, the translocation guards) is ongoing cost for a path no real
  deployment needs.

- **Keep the CLI symlink install, drop only the system daemon.** The
  `/usr/local/bin` write is the smaller privileged path, but it is still an admin
  prompt and still complexity (foreign-owner channel classification, the
  orphan-on-launch reconcile, the TOCTOU-guarded escalation) for something the
  Homebrew cask and "run it from the bundle" already cover. Once the boundary is
  "no admin prompt, ever," a single remaining prompt would be the exception that
  keeps the whole broker alive.

- **Rip both** (chosen). The two privileged paths share the broker, so removing
  both is what actually deletes the privileged seam — leaving either one keeps the
  `osascript` admin runner and its escaping/cancel/confirm machinery in the app.

- **Keep the broker, gate it behind a hidden/advanced flag.** Same surface, now
  with a discoverability problem and a config axis; the complexity is in the code
  path, not the button, so hiding the button saves nothing.

## Consequences

- **Supersedes, in part, three ADRs** (amendment banners added to each):
  - [ADR-0018](0018-bundled-app-distribution.md): the **CLI-vending** decision
    ("Vend the CLI by symlinking `/usr/local/bin/runnyctl`") is withdrawn — the
    app no longer installs the CLI. Bundling the binaries, the per-user
    LaunchAgent, version-lock, loud skew, and the system-daemon detect-and-defer
    *observation* all stand.
  - [ADR-0020](0020-headless-system-daemon.md): the **app path** for installing
    the system daemon ("The app shells to the same subcommand") is withdrawn —
    only `runnyctl install-daemon` (operator-run) installs it. The account, home,
    ACL, and plist decisions are unchanged.
  - [ADR-0022](0022-app-driven-runnyd-updates.md): the **app-brokered re-stage**
    of the system daemon's binary is withdrawn — the headless `runnyctl
    upgrade-daemon` path is now the only system-daemon update. The config-compat
    gate, the OK/Warn/Error verdict, and the per-user auto-apply are unchanged.

- The app target loses `PrivilegedBroker`, `SystemDaemonInstaller`, `CLIInstaller`,
  and `CLIInstallPlan` (and their tests) — a net deletion. The `isTranslocated`
  bundle-path heuristic, which is not privileged, is relocated to a small plain
  util the per-user eligibility check reuses.

- The `systemManaged` ownership verdict and its observer banner stay (the system
  probe is unprivileged); only the banner's remediation text changes to name
  `runnyctl`. The spawn gate still denies a per-user install over a `systemManaged`
  daemon, so the app never creates a competing manager.

- `docs/deploy.md` drops the app's CLI/system-daemon install instructions; the
  app section becomes "per-user agent + observe a system daemon." `runny-app.md`
  drops the CLI-vending and system-daemon-management sections.
