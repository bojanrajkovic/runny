# ADR-0021: Sharing one image pull across concurrent slots

**Status:** Accepted (2026-06-21)

## Context

Every slot in a pool enters ENSURE_IMAGE together on a cold start and needs the
same image. The byte-pull was already single-flighted — `oci.PullTo` holds a
per-destination lock, so one slot pulls while the rest wait and take the cache
hit. What was *not* shared is the **outcome** of a pull.

Pull failures split in two. A **transient** failure (a registry blip, a stalled
transfer) is worth re-attempting: the next try might succeed, so each slot
retrying on its own backoff is correct. A **deterministic** failure fails
identically for every slot until host state changes; the clearest case is the
pre-flight disk-headroom guard — if the uncompressed image plus its headroom does
not fit, no slot can ever succeed until disk is freed. Under the old code the
waiter inherited none of that: it acquired the released lock and re-ran the
identical guaranteed-failing pull, re-resolving the digest, then cycled its own
FSM backoff. With N slots that was N doomed attempts per round, N registry
round-trips, and N slots all showing `ENSURE_IMAGE: pulling…` while none could
succeed. Surfaced live: two slots churning the same disk-guard refusal.

The fix has to share the deterministic failure without (a) reintroducing an
unbounded operation ([ADR-0011](0011-bounded-contexts.md) — a doomed pull
retried forever is the silent-hang shape this project exists to kill) and
without (b) taking long-run retry ownership away from the slot FSM
([ADR-0004](0004-destroy-and-recycle-state-machine.md)).

## Decision

**A per-destination image-puller actor shares one in-flight pull and its
outcome.** A slot resolves its digest and checks the cache per-slot, then
subscribes to the actor keyed by the image bundle directory and blocks on the
shared result. The directory is content-addressed (`home.ImageBundleDir` embeds
the ref and the resolved digest), so every subscriber of one directory
necessarily wants the same bytes — the key needs nothing more. Resolve stays
per-slot because the digest is what *produces* the key.

**The deterministic disk failure holds and polls; it does not re-pull.** The
disk guard returns a typed `oci.DiskHeadroomError` carrying the bytes the pull
needs. On that error the actor enters a bounded hold: it polls free space against
that figure on the filesystem the guard checks, and re-attempts the pull only
once there is room. Subscribers stay in a single shared
`waiting: insufficient disk headroom, retrying` state — the held line carries the
elapsed time so it refreshes past the status layer's identical-string dedup —
instead of each falling into its own backoff.

**Transient failures are broadcast; long-run retry stays with the FSM.** A
non-deterministic error is delivered to every subscriber, which then retries via
its own FSM backoff, unchanged. The deterministic hold is bounded by a
wall-clock budget; when it elapses the actor gives the failure back to every
subscriber, so the FSM still owns the long-run loop. The failure streak ticks
**once per give-up window** rather than once per underlying attempt — N×(slots)
doomed attempts collapse to one increment per window.

**The concurrency contract is a first-class invariant.** Terminal outcome and
the subscriber set live under the actor mutex; subscribe, finish, and last-out
teardown are serialized so a late subscriber either joins before the broadcast or
starts a fresh actor — it can never block on a dead one. Lock order is always
registry → actor; status callbacks run outside the actor lock (they take a slot
lock, so holding the actor lock across them would invert the order). A panic in
the pull is converted to a terminal error, because a subscriber hanging on a
crashed actor is precisely the failure mode the project forbids.

The hold budget is sized as a standalone healthy magnitude — long enough for an
operator to free disk — not derived from any other budget, per the
[bounds principle](../architecture/bounds.md).

## Rejected alternatives

- **A shared per-destination "last deterministic failure" cache.** A waiter that
  found a recent cached failure would short-circuit with it instead of
  re-pulling — ~20 lines, and it keeps backoff wholly in the FSM. But it does not
  give the shared-waiting experience: each slot still transitions to BACKOFF and
  shows its own "backoff: insufficient disk" rather than one shared, live
  "waiting: disk full, retrying" line. It removes the wasted pulls but not the
  divergence, and the actor's disk-poll is in fact *less* registry churn (one
  manifest fetch, then free-space syscalls).

- **The status quo plus nothing.** Already the bug: every concurrent slot
  re-runs the doomed pull and churns its own backoff.

- **Moving all retry into the actor / retrying a doomed pull forever.** Waiting
  on disk indefinitely is an unbounded operation; an operator who never frees
  space would leave every slot wedged behind an actor with no recycle and no
  failure accounting — the exact silent stall ADR-0011 forbids. The hold is
  bounded and hands back to FSM backoff, which keeps the visible recycle and
  streak intact.

- **Re-running a full pull on each deterministic retry.** A pull re-fetches the
  manifest and re-runs the guard; polling free space against the bytes the first
  failure already reported is a cheap syscall and avoids the round-trip until
  there is genuinely room.

- **A composite actor key (registry + repository + digest + …).** Unnecessary:
  the bundle directory is already content-addressed, so two subscribers share a
  key only when they want byte-identical images. A moved tag resolves to a
  different digest, hence a different directory and a separate actor.

## Consequences

- Concurrent slots behind a doomed pull show one shared, live "waiting" state and
  run one pull attempt per round instead of N; a deterministic disk failure is
  legible as failure-adjacent (it names the shortfall and the elapsed wait), not
  as fake progress.
- The FSM failure streak advances once per give-up window, not once per
  underlying attempt; long-run backoff/recycle ownership is unchanged.
- The actor adds a concurrency surface (a refcounted registry, a lifetime
  context cancelled at the last subscriber, a panic-to-terminal guard) covered by
  tests for shared outcome, disk hold-then-recover, bounded give-up, transient
  passthrough, last-out teardown, recycle-one-keeps-alive, panic, and the
  registry race — all under the race detector.
- `oci.PullTo`'s per-destination lock stays as defense in depth; the actor is the
  single production caller, so it never contends. The per-slot "waiting" pull
  annotation is gone — the actor owns wait visibility now. The runner-tarball
  ensure is unchanged (its failures are not the shared-deterministic-hold case).
