# ADR-0020: Headless runnyd via a non-root system LaunchDaemon

**Status:** Accepted (2026-06-19)

Supersedes the networking-contingency framing of
[ADR-0018](0018-bundled-app-distribution.md) (see its 2026-06-19 amendment): a
headless, non-root runnyd is viable today, with no change to the network
substrate.

## Context

runny shipped one install shape: a per-user **LaunchAgent**, started by launchd
in a GUI login session ([ADR-0018](0018-bundled-app-distribution.md),
[ADR-0016](0016-runny-app.md)). That requires a desktop login — unworkable for a
headless fleet host. The blocker was believed to be the network: a daemon not in
a GUI session was thought to be denied macOS Local Network access (the vmnet
subnet runnyd reaches guests over).

A spike on a pristine, never-logged-in host plus Apple's TN3179 overturned that:
**a daemon started by launchd is auto-allowed Local Network access regardless of
uid.** The *gated* path is the per-user LaunchAgent, because macOS keeps
Local-Network privacy state per user account (hence its one-time prompt); the
genuinely denied case is a process that self-daemonizes / reparents away from
launchd. So the lever is not "move off vmnet" — it is **run as a launchd-started
system daemon.** vmnet stays. (The userspace network stack that originally framed
this work is demoted to optional hardening,
[#84](https://github.com/bojanrajkovic/runny/issues/84).)

Two shapes can deliver a system daemon but should not:

- **`sudo brew services`** runs the daemon as **root** — unnecessary privilege at
  runtime, the thing to avoid.
- **`brew services` (no sudo)** is *necessarily* a per-user agent: creating a
  *system* daemon means writing `/Library/LaunchDaemons` and `launchctl bootstrap
  system`, both privileged, and Homebrew's `service` DSL has **no run-as-user
  field** — it cannot run a daemon as a dedicated non-root account at all.

A one-time privileged *install* is unavoidable for any system daemon (an OS
constraint). The decision is how to keep *runtime* unprivileged, and where the
daemon's state lives when its run-user has no home.

## Decision

**Install runnyd as a non-root system LaunchDaemon under a dedicated service
account, privileged once at install, unprivileged at runtime.**

- **Dedicated service account `_runny`.** Hidden, no login shell, and **no home
  directory** (`NFSHomeDirectory=/var/empty`). Its uid/gid are auto-allocated
  from the system range (200–400) rather than pinned — collision-safety across a
  fleet over byte-identical ids. The plist carries `UserName=_runny`; runnyd runs
  as that account.

- **The home is `/Library/Application Support/runny`, deployment-resolved** (not
  the account's `$HOME`). The system daemon keeps its *entire* state there —
  config, the App key, logs, images, VM clones, cycle artifacts, and the control
  socket — so the service account needs no home of its own. `runnyd` resolves its
  home by **ownership** (it must own the tree it binds); clients (`runnyctl`, the
  app) resolve by **existence** (they reach it through the ACL, never owning it).
  Resolution is consistent across daemon and clients, with no env/flag override,
  so they can never disagree about where the socket and credentials live (this
  preserves the no-switchable-home invariant of the fixed-`~/.runny` decision,
  now generalized from "one literal path" to "one deterministic resolution").

- **A dual inheriting ACL on the home** is the access boundary, over a `0700`
  POSIX mode owned by `_runny`. The **operator** account gets directory write +
  read (edit config, atomically rename over it, land the App key, read artifacts,
  reach the socket); **`_runny`** gets read (so the daemon can read an
  operator-landed `0600` config and key it does not own). macOS evaluates an
  allow ACE ahead of the POSIX mode, and the ACEs inherit onto every file either
  party creates. Both ACEs are load-bearing — without the `_runny` ACE the daemon
  cannot read its own config/key; without the operator ACE the operator cannot
  manage the daemon without `sudo`.

- **Install via `runnyctl install-daemon` (one `sudo`)** — it creates the
  account, the home + ACL, the plist, and `launchctl bootstrap system`. The app
  shells to the same subcommand (ADR for the app path is PR5's). The plist points
  at the `runnyd` sibling of the running `runnyctl` (the stable Homebrew opt
  symlink, which survives `brew upgrade`); `tools/deploy/install-system.sh` stages
  the binaries and delegates to the subcommand for the from-checkout case, so the
  privileged steps have one implementation.

- **The Homebrew formula becomes delivery-only** — it ships the binaries and
  drops its `service` block (it could only ever make a per-user agent).

- **The launchd-started invariant is load-bearing and asserted.** The
  auto-allow holds only while runnyd never self-daemonizes; the crash-only
  KeepAlive model already guarantees this, and a startup check plus a regression
  test enforce it.

## Alternatives weighed

- **Root LaunchDaemon (`sudo brew services`, or a root plist).** Simplest, but
  runs at root — gratuitous privilege for a daemon that needs only vmnet and its
  own home. The `UserName` non-root daemon is strictly better and no harder to
  reach.
- **Per-user LaunchAgent for headless hosts.** The shipping shape, but it needs a
  GUI login for the one-time Local Network prompt — the exact constraint headless
  must drop. Kept as the *desktop* path, not headless.
- **Pinned service-account uid.** Byte-identical across a fleet (config-management
  friendly), but risks colliding with an id a host already uses; auto-allocation
  is safer and the home's ownership (which resolution keys on) is re-asserted on
  reinstall regardless.
- **Single (operator-only) home ACL.** Fails the spike: a `0600` config/key the
  operator lands is operator-owned, and the daemon — a different uid, not the
  owner — cannot read it. The second `_runny` read ACE is required.
- **An out-of-home config (`runnyd -config /etc/...`).** The earlier plan, to
  dodge writing into the account's home. Mooted by putting the whole home in
  Application Support with the operator ACL: config lives in the home and the
  operator edits it directly, no split location.
- **Resolving the plist's runnyd path via `EvalSymlinks`, or a `--runnyd`
  override flag.** EvalSymlinks pins the versioned Cellar path and orphans the
  plist on `brew upgrade`; the flag is unneeded since sibling resolution covers
  both shipping channels and the script handles from-checkout. Neither is used.
- **Userspace network stack (gvisor-tap-vsock) to sidestep vmnet/TCC entirely.**
  Viable and structurally immune to the gating edge cases, but unnecessary for
  headless now that the launchd-auto-allow finding stands; deferred as optional
  hardening ([#84](https://github.com/bojanrajkovic/runny/issues/84)).

## Consequences

- A headless host installs with `brew install` + `sudo runnyctl install-daemon`,
  no desktop login. The operator-facing procedure is in
  [deploy.md](../deploy.md) ("Headless system daemon"); the access posture is in
  [security.md](../security.md) ("Headless system daemon").
- The installer's account creation, home ownership, and the two ACEs are
  security-load-bearing; the install logic lives in one place (`internal/sysdaemon`)
  with the privileged steps behind a tested seam.
- Existing `brew services start runny` users lose the auto-registered agent on
  their next formula upgrade and must register explicitly (system daemon, or the
  app's per-user agent). This is a deliberate migration, not a silent break.
- Uninstall **purges the home** (keeping only the account): because clients
  resolve the system home by existence, a preserved home would keep winning
  resolution and make a later per-user agent unreachable, and it would leave the
  App key at rest. Uninstall verifies the job is unloaded before removing
  anything, so it never reports success over a still-running daemon.
