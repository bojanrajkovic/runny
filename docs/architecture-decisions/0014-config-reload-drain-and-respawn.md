# ADR-0014: Config reload — validated drain-and-respawn through the supervisor

**Status:** Accepted (2026-06-11)

## Context

runnyd reads `~/.runny/config.yaml` exactly once, at startup. Any config
change required a daemon restart, and a bare restart cancels the root
context: in-flight cycles fail into TEARDOWN, which kills running jobs. The
only graceful path was a manual runbook (pause every slot, wait for paused
BACKOFF, restart) — and nothing validated the new file before the restart
applied it, so a typo crash-looped the daemon under launchd's KeepAlive
with the fleet already drained.

The daemon already has a supervisor that makes restarts free: launchd
KeepAlive respawn is load-bearing for the ADR-0012 wedge escalation.
Restarting through it applies the new config as a crash-only cold start —
reload reuses the restart, it is not a new mechanism.

## Decision

A `Reload` RPC (runnyctl and RunnyBar stay symmetric, ADR-0006) plus SIGHUP
mapped to the same path:

1. **Full-gauntlet preflight.** Validation runs everything a cold start
   runs, in order: `config-parse` (strict parse + defaults + validate),
   `github-client:<pool>` (client construction — which happens *before*
   the doctor suite at startup, so a deleted private key would pass a
   parse-only check and crash-loop the respawn), then the whole doctor
   suite against the candidate config. Refusal is loud, structured (the
   failing checks ride the response), and leaves the running daemon
   untouched. Two environment-sensitivity adjustments: a failing
   `local-network` is a warning, not a refusal (it only asserts with a
   vmnet interface up — true at reload time, false at the respawn's cold
   start — and no config edit affects it); `disk-headroom` stays a refusal
   but its detail says the number was measured with guests running and the
   respawn sweeps clones before re-checking.
2. **Drain via the shared drainer.** On acceptance the existing ADR-0012
   machinery drives every slot to a stable state (pause + recycle; running
   jobs finish first; convergence = wedged or paused-in-BACKOFF). Zero new
   FSM states or commands: reload is an orchestration of the state
   machine, not a mode of it. The drainer re-issues commands on every
   status change, making convergence monotone (and fixing a latent
   dropped-command/timer-race stall in the issue-once wedge drain).
3. **Local exit gate, then exit non-zero.** At convergence, before handing
   the process to launchd, the drainer re-parses the on-disk file (local
   I/O only — no network at the exit seam). Parse failure HOLDS the exit:
   the daemon stays up, drained, serving status with the hold annotation,
   and a 30s ticker revalidates so fixing the file is sufficient. A hash
   change against the accepted SHA exits with a WARN naming both hashes.
   Both drain causes share the gate — a wedge respawn loads the same file,
   and holding beats crash-looping there too. The exit is always non-zero
   (`restarting after drain: …`) so respawn never depends on launchd
   treating success exits as restart-worthy.
4. **Cold start sweeps the vms dir before validating.** Teardown retains a
   wedged guest's clone, whose divergence can tip `disk-headroom` under
   the threshold; with validation first, every respawn failed before
   reaching the sweep that would free the space — a non-self-healing crash
   loop. The sweep depends on nothing, runs only on the real-startup path
   under the instance lock, and never in `-doctor` mode (read-only against
   a live daemon).
5. **Reload during an active drain validates anyway.** If the handler
   short-circuited with "already draining", the operator would read
   *refused* as *not accepted* — but the imminent respawn loads the
   on-disk file regardless, validated or not. So the preflight always
   runs; an active drain just means no second drain starts, and the
   response says which drain will apply the config (or screams that the
   respawn WILL load the invalid file).
6. **No drain budget.** Convergence is bounded by composition (the
   ADR-0012 argument): every pre-LISTENING state carries a deadline,
   recycle ends LISTENING immediately, JOB is bounded by
   `limits.max_job_duration`, TEARDOWN by `deadlines.teardown`. A
   reload-specific timeout could only fire by killing a job — the precise
   failure mode this feature exists to avoid — or be dead code. Visibility
   replaces policy: the `draining` status field plus per-slot states name
   the holdout, and `runnyctl recycle` remains the audited human override.
7. **Pause is not persisted.** Pause is an in-memory operator hold;
   persisting it would put directives on disk that cold start must honor,
   against ADR-0004's "cold start owns the world", and would require
   distinguishing operator-pause from drain-pause. Instead: the daemon
   WARN-logs operator-paused slots at acceptance, the response carries the
   list, and `Pause` during a drain returns a note that the pause will not
   survive the respawn.
8. **The audit trail is provable.** The acceptance log records the
   validated file's SHA-256; every cold start logs the SHA-256 of the file
   it loaded; the per-slot recycle reason (`draining for config reload
   (<source>): <reason>`) lands in each interrupted cycle's record. A new
   `config-drift` doctor check (post-defaulting struct equality) makes
   "the file differs from the running config" visible any time.

### The guarantee, stated honestly

Full-suite preflight parity guarantees a reload **cannot drain the fleet
into a KeepAlive crash loop for any defect present at validation time**.
It is *not* a guarantee against drift during the drain (bounded only by
`limits.max_job_duration`): a credential revoked server-side, a tag
deleted from the registry, or disk filled by running jobs fails the
respawn's own startup validation exactly as it would fail any bare
restart. Transient causes self-heal (every KeepAlive retry revalidates);
non-transient drift needs operator action and is diagnosable in
`launchd.err.log` and `runnyd -doctor`; the hash chain proves whether the
file the respawn loaded was the file the preflight vetted. The local exit
gate closes the largest controllable slice — the file itself edited to
garbage mid-drain.

## Rejected alternatives

- **In-place hot reload (pool diffing, dynamic slot rebuild)**: pools can
  change arbitrarily (count, image, target, App); slots would gain a
  reconfiguring state and the socket server's slot list would mutate under
  live watch streams — the "modes without value" complexity ADR-0004
  rejects.
- **Blind-restart guidance**: kills in-flight jobs, validates nothing,
  crash-loops on a bad file.
- **SIGHUP-only**: no synchronous verdict for the caller, asymmetric with
  RunnyBar (ADR-0006).
- **Exit immediately on reload**: kills jobs for a condition that can wait
  (the ADR-0012 argument, again).
- **A drain budget / forced cutover**: reintroduces reload-kills-job, or
  is dead code.
- **Full network re-validation at exit time**: would narrow the TOCTOU
  window, but a network refusal at the exit seam has no good answer (hold
  a drained fleet on a GitHub blip?) and reintroduces network work at the
  exact moment the daemon must exit. The local gate covers the file-defect
  slice; the rest is accepted risk.
