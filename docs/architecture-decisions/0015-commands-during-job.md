# ADR-0015: Operator commands are serviced during JOB; recycle cancels a running job only with explicit consent

**Status:** Accepted (2026-06-12)

## Context

Before this change `watchJob`'s select had exactly two arms — `ctx.Done()` and
`proc.Lines()` — and no command arm. An operator command issued mid-job sat in
the 8-deep buffer until the cycle ended, and what happened next was a latent
bug in both branches:

- If the slot then waited in BACKOFF, `backoffWait` drained the buffer and
  `handleIdleCommand` **silently dropped** a `CmdRecycle` ("nothing to recycle
  while idle").
- If backoff was zero (a healthy slot), `backoffWait` returned immediately
  without draining, and the **next cycle's** runCycle watcher dequeued the
  stale recycle and cancelled a healthy boot that did nothing wrong — the exact
  failure class the watcher's own comment memorializes.

Mid-job injection (ADR-0014) makes servicing commands during JOB a hard
requirement — the operator must be able to install a key into a running job —
so the latent bug must be fixed rather than worked around.

## Decision

**`watchJob` gains a `case cmd := <-s.cmds` arm; operator commands are serviced
during JOB, with explicit semantics per kind.**

| Command | Mid-job behavior |
| --- | --- |
| `CmdRecycle` with `CancelJob` | cancels the running job into TEARDOWN immediately (audited operator override; `jobRan=true`) |
| `CmdRecycle` plain | disarms any armed debug hold (audited, live status cleared), logs Warn; the job finishes and tears down as it structurally always does |
| `CmdPause` | `setPaused(true)` immediately; a paused slot never enters a post-job DEBUG hold, and pausing while armed disarms now (audited) |
| `CmdResume` | `setPaused(false)` immediately |
| `CmdDebugKey` | inject-only + arm; the runner is never touched (ADR-0014) |

The asymmetric recycle split exists because "defer the recycle to job end" is a
**structural no-op**: JOB always exits through TEARDOWN, so a plain recycle's
destroy outcome is already guaranteed at job end — the only real question is
whether to destroy *the job too*, which needs explicit consent
(`RecycleRequest.cancel_running_job`, set by runnyctl's `-force` guard only
after *observing* JOB). The wedge drain (`cmd/runnyd/main.go`, plain
`CmdPause`+`CmdRecycle` to every slot, documented contract "a running job
finishes first") never sets the flag and keeps its semantics with zero code
changes.

The residual stale-recycle window — a plain recycle enqueued during the
consumer-less TEARDOWN window — is closed for real: `backoffWait` **drains and
discards any buffered `CmdRecycle` before arming its timer** (a recycle of a
slot that no longer exists has nothing to recycle), so a teardown-stranded
plain recycle can never reach the next cycle.

## Alternatives considered

- **Keep buffering** (rejected): the verified latent bug — a silently dropped
  recycle, or a stale recycle destroying the *next* healthy cycle.
- **Kill the running job by default, guarded only client-side** (rejected): a
  raw-RPC default that kills CI jobs is a loaded gun for owner automation, and
  the **raced recycle** (an operator recycles a slot that status said was
  LISTENING; the job starts before dequeue) would destroy a job the operator
  never consented to kill. The consent flag is set only after *observing* JOB.
- **Reject all commands in JOB** (rejected): leaves the operator no lever to
  kill a genuinely runaway job before its budget — the same incident class
  mid-job injection serves.
- **A status-sniffing drain** (rejected): default-safe semantics (plain recycle
  disarms-only mid-job) close the scan-to-dequeue race by construction, with no
  status read at send time.

## Consequences

- Command semantics change fleet-wide, independent of the debug feature — hence
  this separate ADR. `docs/architecture/runnyd.md` and the `recycle` help text
  call it out.
- `CmdPause`/`CmdResume` now take effect *immediately* mid-job; `Paused: true`
  appears in `status` during a job.
- Issue #42's reload drain inherits all of this by sending the same two
  commands.
