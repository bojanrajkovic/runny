# ADR-0017: Confirming reload convergence by process identity

**Status:** Accepted (2026-06-15)

## Context

A config reload drains the fleet and exits for a launchd cold start
([ADR-0014](0014-config-reload-drain-and-respawn.md)). launchd's `KeepAlive`
restarts the daemon on **any** exit, so a socket that reappears is ambiguous: it
could be the respawn on the new config, a crash restarted on the *old* config,
or a respawn on a file someone re-edited mid-drain. A client that reports "the
socket came back" as success cannot tell a converged reload from a crash loop.

Confirming a reload means identifying *the* respawn — a genuinely new process, on
*the* config the preflight vetted — positively, from published signals, not from
mere reachability. Two questions have to be answered over the wire: "is this a
different process than the one I asked?" and "did it load the file I validated?"

## Decision

**Identify the respawn by a per-process identity, not a timestamp.** The daemon
mints a random `boot_id` at every cold start (in memory, never persisted —
distinct from the persisted instance-id, which names the host's runner family
and is stable across respawns). It publishes `boot_id` in `GetStatus` and echoes
the **accepting** process's `boot_id` back in the `Reload` response. A follower
baselines on that echoed id and waits for a status reporting a *different* one.
Because the baseline rides the reload response, there is no earlier read whose
process could differ from the one that accepted — the identity is positive and
the sub-RPC race is closed by construction.

**Prove the config with `config_sha256`.** The running daemon publishes the hash
of the file it loaded at cold start; the follower compares it against the hash
the `Reload` response returned. A new `boot_id` plus a matching hash is the whole
positive fingerprint. Config drift — a different hash — is the one actionable
failure.

**Bound the drain by daemon-authoritative progress.** The daemon bumps a
`drain_seq` counter on each real drain event (a slot transition, an exit-gate
hold flip), never on the periodic status heartbeat. A follower resets its
progress-stall only when `drain_seq` changes, so a wedged-but-heartbeating daemon
can no longer mask a stalled drain. The stall is suppressed while a slot is in a
state whose own bound is delegated daemon-side (a running job, a progress-bounded
pull) or while the exit gate is held.

**Size the waits as backstops, never sums.** The follow path uses three
magnitude-matched clocks, never summed and at most one armed at a time — an
establish backstop, a reload-preflight backstop, and a respawn cap armed only
once the daemon is confirmed gone. The sizing follows the
[bounds principle](../architecture/bounds.md).

The living shape — the fingerprint, the two-phase bounding, the verdict — is
[reload-convergence.md](../architecture/reload-convergence.md).

## Rejected alternatives

- **`daemon_started` (a timestamp) as the discriminator.** A timestamp the old
  process also holds cannot prove "new process," so the client must capture a
  baseline *before* the reload RPC — and that pre-RPC read is a race: a respawn
  between the read and the RPC makes the baseline the wrong process's. The
  timestamp also forces a probe phase to interpret a dropped stream and a
  breadcrumb to recover the graceful-exit signal. Every one of those is a symptom
  of one wrong primitive; a real per-process identity dissolves them. The
  timestamp is retained only as a display value and as the degraded fallback for
  a daemon too old to report a `boot_id`.

- **An on-disk clean-exit breadcrumb** (the dying process writes a file the
  successor consumes) to distinguish a graceful drain-exit from a
  crash-during-drain. It is load-bearing on-disk handoff state that the
  crash-only invariant ([ADR-0004](0004-crash-only-state-machine.md)) works to
  avoid, and it tied the success signal to the clean-exit path — making a crash
  that lands the *right* config report a false failure. The actionable
  distinction (a job was interrupted) is instead **observed client-side**: a
  follower watched the drain, so whether a job was still in flight as the daemon
  went down falls out of the snapshots it already received, with no new wire
  field and no handoff state.

- **Deriving the respawn cap from the daemon's startup envelope** (per-check
  budget × pool/client count). This sizes the client wait to the sum of degraded
  budgets, treating "every check timing out" as the baseline and hiding a slow
  cold start behind a multi-minute horizon — the anti-pattern the bounds
  principle forbids. A flat backstop over a healthy cold start is the correct,
  fast verdict.

## Consequences

- One monotonic protocol-version bump gates the new fields; against an older
  daemon a follower degrades to the timestamp discriminator and warns, loudly,
  that it cannot positively confirm.
- The verdict is a small pure function over published signals, mirrored in
  `runnyctl` and the app's `DaemonStore` and unit-tested without a live daemon.
- "Was a job interrupted during the drain" is visible only to a client that
  followed the drain; one that connects late reports the convergence honestly
  and notes it did not observe the drain.
