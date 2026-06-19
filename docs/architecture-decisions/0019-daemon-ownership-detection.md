# ADR-0019: Detecting who manages the daemon

**Status:** Accepted (2026-06-18)

## Context

[ADR-0018](0018-bundled-app-distribution.md) decided the *policy*: the app
installs its per-user LaunchAgent **only when no other manager owns the daemon**,
and otherwise drops to an observer posture and points the operator at the
managing channel. That leaves a *mechanics* question with real reliability and
macOS-version fragility: how does the app learn who owns the daemon?

Three facts are orthogonal and must not be conflated: a daemon may answer the
socket (something runs) without the app knowing who started it; an agent may be
registered under a label the app cares about (`homebrew.mxcl.runny` for the brew
service, `com.coderinserepeat.runnyd` for both the manual installer **and** the
app's own agent); and if the canonical label is registered, it may or may not be
the app's. The manual installer and the app's own agent share the canonical
label in the same `gui/<uid>` launchd domain, so a label match alone cannot tell
"mine" from "theirs". A detector that gets this wrong does the exact thing the
phase exists to prevent: install a second manager over a Homebrew daemon, or tell
an operator to stop a hand-run daemon that a probe merely failed to see.

## Decision

**Use `SMAppService` for self-identity and a bounded, targeted `launchctl` probe
for foreign detection.** Two sources, each authoritative about a different thing:

- **Self-identity — `SMAppService.agent(plistName:).status`.** `.enabled` means
  the app owns the canonical label (`selfManaged`); `.requiresApproval` means the
  app's agent is registered but unapproved (`awaitingApproval`); anything else
  (`.notRegistered`/`.notFound`) is not-self and falls through to foreign/unmanaged.
  A spike confirmed the load-bearing fact: a *foreign* `launchctl bootstrap` of
  `com.coderinserepeat.runnyd` does **not** flip the app's `SMAppService` status to
  `.enabled` — that status reflects the framework's own registration database,
  which a plain `launchctl bootstrap` (what `brew services` and the manual
  installer use) never writes. So self-status is an authoritative "is this label
  mine?" signal, and the shared label is disambiguated by it, never by a match.

- **Foreign detection — one bounded `launchctl print gui/<getuid()>/<label>` per
  label**, resolving registration by a **literal-label substring search in
  byte-capped stdout**, not by exit code and not by parsing the dump's format.
  Exit status is ambiguous (the same nonzero covers an absent label, a nonexistent
  domain, a malformed selector, permission/SIP edges). A registered job — running
  *or* stopped — prints a block whose first line is `gui/<uid>/<label> = {`, so the
  literal label is always in stdout, which is what catches a `brew services stop`
  daemon that an exit-status read would miss. An absent label yields empty stdout
  and echoes the label only in *stderr* (`Could not find service "<label>"…`), so
  the search is stdout-only — searching the combined stream would false-positive on
  every absent label. A clean "could not find" is `notRegistered`; a timeout,
  launch failure, or any other error is `indeterminate`, never a false absence.

The verdict is a pure function over these inputs with **`indeterminate`-dominant
precedence over all but one signal**: a non-canonical home or any inconclusive
probe defers ahead of naming a foreign owner, stopping a hand-run daemon, or
installing — so "not sure who owns this" can never read as
install-a-second-manager or stop-your-daemon. The single exception is
authoritative self-identity: an `.enabled` `SMAppService` status means the app
owns the canonical label (a foreign `launchctl bootstrap` never sets it, as
above), so a wedged foreign-label probe never makes the app defer managing *its
own* daemon. `.requiresApproval` is deliberately **not** such an exception — a
pending agent is not yet running and so cannot attest to the loaded label; it
defers to any inconclusive probe like every other non-`.enabled` state, and only
becomes the approval CTA once both probes confirm `.notRegistered` and the socket
is silent. The probe is bounded in wall-clock
and reaped (SIGTERM, then SIGKILL after a grace, with a detached reaper and
explicit pipe-FD close), so a wedged `launchctl` yields `indeterminate` without
leaking a process or FDs — the no-unbounded-operations invariant applied to the
GUI.

## Alternatives considered

- **`SMAppService`-only.** It is authoritative about the app's *own* registration
  and nothing else — blind to `brew services` and the manual installer, which
  register through `launchctl`. It would see `.notRegistered` plus an answering
  socket and wrongly conclude "unmanaged → install", stomping a brew daemon.
  Insufficient for the core requirement, and therefore disqualified as a
  fallback-under-sandbox too: were the app ever sandboxed, the fallback must be a
  loud degraded mode ("can't detect foreign managers — confirm before installing"),
  never a silent SMAppService-only install.

- **Full `launchctl print gui/<uid>` enumerate-and-parse.** Dumps the whole domain
  and searches it. It sees every agent regardless of registrar, but the whole-domain
  dump is large and its *structure* drifts across releases — the unbounded-format
  parsing this project treats as a silent-failure shape. The chosen arm is a
  *targeted* single-label dump matched by a *literal label*, which neither
  enumerates the domain nor parses structure.

## Consequences

- The shared canonical label is safe to disambiguate by self-status — documented as
  a sharp edge in `apps/Runny/CLAUDE.md` so a future maintainer does not "simplify"
  detection to a label match and reintroduce the stomp.
- The spawn chokepoint gates install/repair/start, but two daemon-affecting paths
  sit outside it and consult the verdict directly instead: **approval** (the user
  enabling the agent in System Settings) — its CTA is suppressed unless the verdict
  is `awaitingApproval`, which itself defers whenever another owner is present — and
  **uninstall** (teardown). Two narrow edges are accepted rather than closed:
  `uninstall()` does not re-gather ownership at the instant of teardown, so a
  concurrent external takeover of the shared label during the open Settings window
  could bootout a foreign daemon; and a hand-run daemon that holds the instance lock
  but has not yet opened its socket reads as `unmanaged` for that sub-second startup
  window. Both require an actor — a second process running `launchctl`, or a human —
  to act faster than is realistic; tracked for an ownership-aware-teardown follow-up.
- Detection covers only the `gui/<uid>` domain. A `system/`-domain LaunchDaemon —
  a `sudo brew services` root daemon today, or the dedicated non-root headless
  daemon planned for fleet hosts (#76) — is NOT detected, yet it is fully
  functional: macOS auto-allows local network access to any launchd-started daemon
  regardless of uid (TN3179), so the app could install a competing per-user agent
  over a working system daemon. A real limitation, not a moot one — extending the
  probe to the `system/` domain travels with that headless LaunchDaemon path.
- The literal-label discriminator is more stable than exit-code reading, but the
  `launchctl print` "not found" presentation and the brew label
  (`homebrew.mxcl.runny`, synthesized by Homebrew, verified against a real `brew
  services` install) remain macOS/Homebrew surface — a tracked maintenance hazard,
  not a one-time guarantee.
- The app is not sandboxed and has no entitlements file, so `Process`-spawning
  `launchctl` is permitted under hardened runtime; no entitlement change is needed.
