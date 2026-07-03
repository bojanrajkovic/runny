# ADR-0014: Debug-key injection — LISTENING freezes, JOB arms; release is recycle

**Status:** Accepted (2026-06-12)

**Amended:** 2026-07-01 — each `injected_keys` entry now also carries the
authenticated peer uid (`SO_PEERCRED`, server-read, not client-forgeable) and
a best-effort username snapshot — the accountability half of this ADR's audit
trail extended to name *which* operator, not just that "an operator" acted.
No change to the authorization model below: it remains socket ownership. See
`docs/security.md`'s "Operator debug keys" section for the current behavior.

**Amended:** 2026-07-01 — the control socket can now grant *several*
operator accounts on the system daemon (`runnyctl operator grant/revoke/
list`), an RPC-driven extension of this ADR's own authorization model: any
operator already holds full daemon control, so an operator granting another
is transitive trust, not a new gate, and the daemon (which owns its home)
performs the grant unprivileged. See `docs/security.md`'s "Operator debug
keys" section for the current behavior.

## Context

When a runner VM misbehaves — a wedged provision, a hung job, a guest that
fails in a way the post-mortem diag does not explain — an operator needs a
shell into the live guest *now*, not an autopsy of its corpse. Runny's guests
are ephemeral (ADR-0004, ADR-0008): the FSM's only response to
trouble is destroy-and-recycle, so by the time an operator notices, the
evidence is usually already gone. Two distinct incident shapes need a live
shell:

- **The idle guest** (LISTENING): something is wrong with the image or the
  provision result, and the operator wants to inspect a clean, job-free guest
  before it picks up work or hits max-idle.
- **The hung job** (JOB): a job is stuck and the operator wants to shell in
  *while it runs* — the running job is the entire point, so it must not be
  touched — and to hold the post-job corpse for inspection rather than let the
  unconditional teardown destroy it.

The credential is the operator's own SSH public key, appended to the guest's
`authorized_keys` alongside the per-cycle key (ADR-0013). It is per-guest,
per-cycle, and dies with the clone at teardown by construction — there is no
key to revoke and no persistence to clean up.

## Decision

`runnyctl debug <slot> [-pubkey FILE] [-hold DUR] [-reason TEXT]` sends one
`InjectDebugKey` RPC. The socket server canonicalizes the key (re-marshals to a
`type base64` line — no client bytes reach a guest shell), validates the hold
against `limits.max_debug_hold`, and enqueues a `CmdDebugKey` on the slot's
existing command channel, pinned to the **CycleID** and **state** the operator
saw. The FSM is single-goroutine, so exactly one select owns the command
channel at any instant, and *which consumer dequeues the command determines its
semantics*:

- **LISTENING → freeze → DEBUG.** Write-ahead audit → drain-check for a raced
  job marker → **verified in-guest runner kill** (`Guest.StopRunner`, a pgrep
  read-back loop that proves the listener dead) → drain output to channel-close
  → bounded `authorized_keys` append with a grep read-back → enter a frozen
  **DEBUG** state. DEBUG is exempt from max-idle and structurally unable to take
  a job (the listener is *proven* dead and JIT configs are single-use).

- **JOB → install now + arm.** Write-ahead audit → bounded install over the
  cycle's live SSH client (a fresh channel; the runner proc and the job are
  untouched) → immediate reply (the operator can ssh in *now*) → an **armed
  hold** recorded in memory. At job end (completion, failure, or budget
  expiry), an armed, unpaused slot runs the same verified kill — now legitimate
  because the job is over or forfeit — and enters the same DEBUG hold, the
  clock starting **at DEBUG entry**.

- **DEBUG → extend / add a key.** Re-issuing `debug` in DEBUG re-arms the timer
  — a pure exec-free re-arm when the fingerprint is already installed this
  cycle (survives a guest reboot), or a fresh audited install for a new key.

Release is always destruction ("resume listening" would be
repair-in-place): explicit via `runnyctl recycle` (client-side `-force` guard),
automatic via hold expiry (`limits.max_debug_hold`, default 2h), or daemon
shutdown. All exits go through the unchanged TEARDOWN sink, so the injected key
dies with the clone.

Authorization is socket ownership (0600), deliberately: the socket owner
already transitively holds everything injection grants. The step-up is
*accountability*, answered by a three-layer **write-ahead** audit trail (the
DEBUG StateRecord = the hold window; `cycle.Record.InjectedKeys` = one entry
per attempt with FSM state and outcome; an `operator-access.json` sidecar
written before any byte reaches the guest and surfaced in `runnyctl why` even
after a crash).

## Alternatives considered

- **Inject without freezing an idle guest** (rejected): a key in a non-frozen
  guest races max-idle teardown and job pickup. In LISTENING, injection *is*
  freezing.
- **Freeze via session close instead of a verified kill** (rejected):
  `Proc.Kill` only closes the client-side SSH channel; no signal reaches the
  remote process and sshd does not kill a non-pty child on channel close, so
  the idle listener would stay ONLINE and job-eligible for the whole hold.
  DEBUG entry requires a proven kill (pgrep read-back) or it refuses.
- **Eager `DeleteRunner` at freeze** (rejected): couples an incident tool to
  GitHub availability. Deregistration happens at teardown via the existing
  `!jobRan` path.
- **Hold clock starts at injection time** (rejected for the mid-job case): for
  the canonical "inject early into a job that runs to its 2h budget, default 2h
  hold", the effective post-job hold would be zero — silently deleting the
  post-job use case. The clock starts at DEBUG entry; the mid-job phase is
  bounded by `max_job_duration`, the DEBUG phase by `max_debug_hold`.
- **Verified kill before the mid-job install** (rejected): killing the runner
  mid-job *is* killing the job, which the prime constraint forbids. The kill
  moves to job end.
- **Auto-convert a raced LISTENING command into a mid-job injection**
  (rejected): it would write contamination into a CI job's permanent audit
  record without the operator's consent. A command whose `SeenState` no longer
  matches is refused with an actionable re-run message (decision: consent is
  pinned to the observed state).
- **A second command-consumer goroutine for JOB** (rejected): breaks the
  single-consumer invariant the timing proof rests on. JOB services the
  existing channel in its own select.
- **Mid-job `Redial`** (rejected): `Redial` best-effort-closes the SSH client
  that carries the running proc; redialing would kill job observation. A
  mid-job `ErrGuestUnreachable` replies "unreachable" and the job continues.
- **Fail a mid-job injection into teardown** (rejected): a mid-job guest's
  future is already terminal (single-use JIT), and an errored attempt never
  arms, so JOB's unconditional teardown resolves the credential ambiguity at
  job end. Destroying it early *is* killing the job.
- **Trust a `Lines()` close as proof of death at the post-job tail**
  (rejected): transport death is indistinguishable from a process exit; the
  pgrep read-back is the only accepted proof.
- **Fail the post-job drain timeout into teardown** (rejected for the tail
  only): post-job the listener is *proven* dead and took its one job, so no
  marker can recur; the residual cause of a non-closing channel is an orphaned
  job descendant holding the inherited stdout fd — the budget-expiry autopsy
  pathology, which must not destroy the corpse. The tail force-closes and
  proceeds.
- **Harvest `proc.Wait()` on the post-job force-close path** (rejected):
  `Wait` blocks until `Client.Close` cuts the socket, which on a held slot is
  up to `max_debug_hold` (2h) away — harvesting it would hang the FSM goroutine
  unboundedly on exactly the orphaned-fd pathology this step exists to tolerate
  (an ADR-0011 violation). The exit code on that path is recorded as
  unknowable.

## Consequences

- **Worst-case slot occupancy is `max_job_duration + max_debug_hold`** (4h
  default) — the price of mid-job arming. Both knobs are operator-tunable and
  occupancy is visible in `status` throughout.
- **The kill proof targets the listener, not job-step processes.**
  `Runner.Worker` and job-step trees do not reliably carry `--jitconfig` and
  may survive the budget-expiry pkill; that is acceptable and partly desirable
  (they are the autopsy subject) — a dead listener plus single-use JIT is the
  no-new-jobs guarantee. Orphaned step processes die at teardown regardless.
- **A job may run with an operator credential present**, by explicit consented
  operator action. The job record itself says so (`JobInfo.operator_keys`);
  every attempt is in `injected_keys` with its state. Runny's contract here is
  visibility, not prevention.
- **A direct RPC bypasses runnyctl's client-side guards.** The socket is
  0600 owner-only; the proto comments and the audit reason are the warning.

Recorded in `docs/security.md`. The command-servicing prerequisite is
ADR-0015.
