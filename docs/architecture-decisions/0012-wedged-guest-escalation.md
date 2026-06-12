# ADR-0012: Wedged-guest escalation — park the slot, restart cold when idle

**Status:** Accepted (2026-06-10)

## Context

Teardown's force-stop is the floor of the crash-only doctrine, but
Virtualization.framework can refuse it: `vzMachine.Stop` genuinely returns
"force stop failed with guest still running". Guests are in-process VMs, so
no amount of in-process escalation reclaims one — only process exit does.
And macOS hard-caps concurrent macOS guests at 2, so an undead guest
permanently eats half the fleet's capacity.

The original code handled this case by lying: it logged the error, deleted
the clone bundle out from under the live guest, recorded the TEARDOWN state
as `ok` in cycle.json, and went back to cycling — every subsequent boot on
that slot failing against the occupied guest cap, with the records swearing
teardown was fine. That is the silent-degradation shape this project exists
to kill (found in the 2026-06-10 whole-codebase review).

## Decision

Three parts, in escalating order:

1. **Tell the truth.** The TEARDOWN `StateRecord` gets `outcome: error` with
   the stop failure; the cycle is recorded as a failure; the clone bundle is
   *not* deleted (the guest holds it, and it is evidence).
2. **Park the slot.** The slot marks itself wedged (`Status.Wedged`, a new
   `wedged` field on the proto `SlotStatus`, rendered as `WEDGED!` by
   runnyctl) and its Run loop exits — re-booting against an occupied guest
   cap just burns doomed cycles.
3. **Drain, then restart cold.** On the first wedge the daemon pauses and
   recycles every slot: pause holds each slot in BACKOFF after its current
   cycle (a running job finishes first), recycle ends LISTENING without
   waiting out max-idle. It exits non-zero only once every slot is in a
   *stable* state — parked-wedged, or paused in BACKOFF, which cannot start
   a job — so there is no scan-then-exit race that could kill a job starting
   mid-scan. launchd (KeepAlive) restarts it; the cold start sweeps the vms
   dir and, the process having exited, the leaked guest is gone. Convergence
   is bounded by the existing max-job-duration budget.

## Addendum (2026-06-11, ADR-0014)

The wedge drain now rides the shared drainer that the config reload
(ADR-0014) also uses: commands are re-issued on every status change until
convergence (the original issue-once drain could stall on a dropped
command or on backoffWait's timer-vs-pause select race), the exit passes a
local exit gate (the on-disk config must still parse — the respawn loads
it for the wedge cause too), and the cold start sweeps the vms dir
*before* validating, closing the crash loop where a wedge-retained clone's
divergence tipped `disk-headroom` under the threshold before the sweep
that would free it could run.

## Rejected alternatives

- **Exit immediately on wedge**: simplest and most crash-only, but kills a
  job mid-run on the healthy sibling slot for a condition that can wait up
  to one job's duration. The fleet is tiny (2 macOS slots); half of it
  staying useful for the tail of a job is worth the small added machinery.
- **Park forever, no exit**: leaves capacity silently halved until an
  operator notices. Loud in `runnyctl status`, but recovery should not
  require a human — the daemon knows exactly what would fix it.
- **Keep cycling the wedged slot**: the status quo minus the lying; every
  cycle fails at BOOT against the occupied cap, generating noise without
  progress.
