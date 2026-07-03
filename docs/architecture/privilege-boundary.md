# Privilege boundary: the app is non-privileged

The Runny app owns exactly one thing — the user's per-user `gui/` launchd domain
— and it raises **no administrator prompt, ever**. Everything that needs `system/`,
root, or a write to a system path is `runnyctl`'s job, not the app's. This is the
canonical statement of that boundary; the decision that enacted it is
[ADR-0023](../architecture-decisions/0023-app-non-privileged-boundary.md).

The boundary is a property, like no-silent-failure and no-unbounded-operations: a surface
either stays inside the per-user domain or it does not, and "inside" is testable —
no `with administrator privileges` string, no `osascript` admin broker, no
privileged subprocess anywhere in the app target.

## Why the line is here

App-brokered privileged management — installing the `system/` LaunchDaemon,
re-staging its binary under an admin prompt, symlinking `runnyctl` into a
root-owned `/usr/local/bin` — was the recurring source of privileged-seam
complexity in the app: a shared AppleScript escaping path, a cancel-a-standing-prompt
process runner, translocation fail-closed guards, exit-code-vs-disk confirmation,
all to drive a one-time `sudo` from a GUI. That complexity served an audience the
GUI does not: a **headless fleet** has no desktop session to click the prompt, and
`runnyctl` already installs and updates the system daemon directly
([ADR-0020](../architecture-decisions/0020-headless-system-daemon.md),
[ADR-0022](../architecture-decisions/0022-app-driven-runnyd-updates.md)). The
desktop user, in turn, never needs a system daemon — the per-user agent is the
desktop shape. So the privileged paths were complexity with no audience on either
side of the split, and the boundary deletes them rather than hardening them.

## What the app may do — all per-user, no prompt

- **Register / start / uninstall the per-user LaunchAgent** via `SMAppService`
  (`gui/` domain). Registration, Login Items approval, `kickstart`, and `bootout`
  are all per-user operations — none raises an admin prompt.
- **Observe and drive a connected daemon**: status, doctor, logs, the cycle
  timeline, slot commands, reload.
- **Surface the Local Network grant** and **version skew** — warnings on the
  per-user agent it manages.
- **Apply a per-user daemon upgrade**: the agent's bundle-relative `BundleProgram`
  already points at the new binary, so a drain-gated reload *is* the upgrade — a
  cold start onto the bundle the app upgrade already replaced, with no privilege.

## What the app must not do

- Raise a `with administrator privileges` prompt, or run any privileged
  subprocess, for any reason.
- Install, update, re-stage, or remove the **`system/` LaunchDaemon**. That is
  `runnyctl install-daemon` / `uninstall-daemon` / `upgrade-daemon`, run by the
  operator.
- Install `runnyctl` to a system path (`/usr/local/bin`). `runnyctl` reaches the
  user's PATH through the Homebrew tap formula or by running it from inside the bundle;
  the app does not vend it.

## What this is not

- **Not a claim about runtime entitlements.** The bundled `runnyd` still carries
  `com.apple.security.virtualization`, and a guest still needs the per-user Local
  Network grant. The boundary is about who raises a privileged *install* prompt,
  not what the daemon is allowed to do once launchd starts it.
- **Not "the app can't see a system daemon."** A system daemon installed by
  `runnyctl` is still **observed** — the app dials the shared socket and streams
  its status as a sibling client, and the unprivileged `launchctl print system/<label>`
  ownership probe drives an observer banner that points the operator at `runnyctl`
  to remove it. Observation is read-only and needs no privilege; the app simply
  never *manages* what it observes here.
- **Not no-silent-failure or no-unbounded-operations.** Those govern how a guest fails
  and that operations are bounded; this governs which launchd domain the app may
  write. They are independent properties that happen to share the silent-failure-proof
  goal.
