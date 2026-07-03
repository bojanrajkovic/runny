# No silent failures

Runny's governing trade-off — silent-failure-proofness over throughput — cashes
out as two guarantees:

1. **A runner slot never fails silently and never gets stuck.** A slot's life
   always resolves *visibly*: it makes forward progress to a completed job, or
   it destroys the guest and recycles with a recorded cause and a visible retry.
   There is no quiet in-place-repair mode for it to limp in, and no path on
   which it records success or looks healthy while it is in fact broken.

2. **The daemon itself is stable.** It never hangs — every guest-facing call is
   bounded ([bounds.md](bounds.md)) — and it exits only deliberately: the last
   rung of a teardown escalation, or a config-reload respawn. launchd
   `KeepAlive` restarts it, and a restart is always a clean cold start that
   recovers the world. Stable means a supervised daemon whose restarts are safe
   and lossless, not a process that never exits.

This doc is the canonical definition the rest of the repo points at. The FSM
that enforces the slot guarantee — the universal TEARDOWN sink, per-state
deadlines, backoff — is decided in
[ADR-0004](../architecture-decisions/0004-destroy-and-recycle-state-machine.md),
and its living transition map is in [runnyd.md](runnyd.md). Keep the two layers
distinct: **no silent failure** is the property; **destroy-and-recycle** is the
mechanism that buys it. A decision is justified by naming one of the mechanism's
clauses below — never by waving at the property in the abstract.

## The mechanism: destroy-and-recycle

Three clauses, each testable against the code:

1. **Destroy-and-recycle, never repair-in-place.** Every failure of a guest
   routes to a single TEARDOWN sink (TEARDOWN → BACKOFF); there is no "diagnose
   and continue" edge. Destruction may be *deferred* by a bounded, visible
   operator hold — a frozen DEBUG guest held for inspection, a paused slot — but
   it is never *replaced* by repair: a held guest still exits only through
   teardown, never back into service. A transient *external* blip is not a guest
   failure and does not destroy: "GitHub unreachable" during the LISTENING
   reconcile holds and logs rather than recycling a healthy runner — keeping it
   distinct from "registration vanished", which does recycle, is the whole point
   of the design. *Test:* the only failure edge out of a working state is
   TEARDOWN; in-state retries and holds wait on readiness or an external
   dependency, they never mutate a broken guest.

2. **Teardown cannot fail — locally.** Teardown escalates force until the guest is
   gone (`RequestStop` grace → `Stop()` → delete); escalating force is the floor.
   When even force-stop is refused in-process, the escape hatch is a *bigger*
   teardown, never a repair: the slot parks loudly and the process exits, and the
   cold start reclaims the leaked guest
   ([ADR-0012](../architecture-decisions/0012-wedged-guest-escalation.md)).
   "Cannot fail" scopes to the *local* destruction; remote side-effects (GitHub
   deregistration) are best-effort and reconciled by the startup sweep. The
   read-only post-mortem that precedes destruction on a failure (a diag tail with
   its own short budget) gathers evidence — it is not repair. *Test:* TEARDOWN has
   no error-out edge; the wedge path terminates in process exit.

3. **A daemon restart is a cold start.** Guests are in-process VMs
   ([ADR-0008](../architecture-decisions/0008-native-virtualization-framework.md)):
   they die with the daemon. launchd `KeepAlive` restarts the process; the restart
   re-validates, sweeps stale guests and offline registrations, and builds fresh
   cycles — it owns the world rather than resuming anything. Disk holds artifacts
   (clones, cycle records, logs) and the *declarative* config, never authoritative
   FSM state and never a runtime directive (an operator pause is deliberately not
   persisted). This clause is *necessary but not sufficient*: in-process state that
   merely dies with the daemon — the app's connection FSM, the socket server — does
   not thereby earn the property. *Test:* nothing on disk is read back as live FSM
   state at startup.

Destruction escalates *outward* through three nested scopes — guest → slot →
daemon. The floor under a teardown that cannot complete at one scope is a teardown
at the next: a guest that won't force-stop parks its slot; a parked slot exits its
daemon; the daemon's cold start finishes the job the in-process teardown could
not. This escalation is why guarantee 2 reads "exits only deliberately" rather
than "never exits": the process exit is the largest teardown, not an instability.

## What counts as visible

The property's teeth are in "visibly". A failure is non-silent when it leaves a
**truthful durable record** — the per-cycle `cycle.json` carries its cause, and
the emitted telemetry carries the same events — *and* the **live status reflects
it truthfully** while it persists: a failing slot's consecutive-failure streak is
surfaced and is never laundered by a benign ending, and an undead guest renders
as a loud `WEDGED!`, never a clean teardown. The bar is *never a success-record
or healthy-status over a real failure* — the precise lie an earlier shape told
(recording teardown `ok` while a ghost guest ate the guest cap) and the one this
property exists to forbid. Actively paging a human is a *monitoring* concern
layered on top, not part of the guarantee.

## What this is not

- **Not a spectrum.** A subsystem either has the property or it does not; there is
  no "more" or "most" silent-failure-proof. Exiting the process is not a *purer*
  form of the virtue, it is just the largest teardown.

- **Not a general doctrine** of simplicity or robustness, and not a slogan. A
  decision is justified by one of the three clauses, named — never by appeal to
  the property in the abstract. Most often it is clause 3: a restart that must
  not interrupt a running job has to drain first, because the in-process guest
  dies with the daemon.

- **Not supervise-and-reconnect of a stream.** Tearing down and re-establishing a
  *connection* on failure — the Runny app's `WatchStatus` supervision — is
  reconnect-on-failure supervision, named in its own terms
  ([runny-app.md](runny-app.md)). A stream is not a VM: its teardown can sit
  unreachable, it carries no in-process guest, and there is no cold start that owns
  the world.

- **Not the single-use / hermeticity boundary.** That each cycle runs a fresh
  clone with no cross-job state is a *security* property — isolation between a
  slot's successive occupants — owned by [security.md](../security.md). It rides
  the same destroy-and-recycle mechanism, which is why the two are easily
  conflated, but it is a different guarantee: no-silent-failure would still hold
  for a reused guest that was recycled visibly on failure.

- **Not the no-unbounded-operations invariant.** This property forbids silent
  *repair*; [bounds.md](bounds.md) forbids silent *hang*. They are the two halves
  of silent-failure-proofness, and they meet at the bounded hold: a "GitHub
  unreachable" wait is safe only because it is both visible (this property) and
  bounded (bounds).
