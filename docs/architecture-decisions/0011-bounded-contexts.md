# ADR-0011: Bounded contexts — the no-unbounded-operations invariant in the type system

**Status:** Accepted (2026-06-10)

**Amended:** 2026-06-11 — added the delegated-bound rule for waits on a
peer's bounded operation (the concurrent-pull and tarball locks).

## Context

"No operation is ever unbounded" was a disciplinary invariant: every
network/guest call takes a `context.Context`, and authors were trusted to
hand it a bounded one. A whole-codebase review (2026-06-10) found four
independent sites where the discipline silently failed, all the same shape —
a context dutifully threaded down with no deadline ever attached:

- ENSURE_IMAGE's manifest `Resolve` ran under the daemon-lifetime context
  (the state is deliberately entered with no wall-clock deadline because pull
  duration is unknowable), while the oci client's `http.Client{Timeout: 0}`
  trusted "ctx bounds them". A registry that accepts TCP and goes silent hung
  every slot forever.
- The startup doctor ran the same `Resolve` under the bare signal context —
  the daemon could hang at boot, before the socket exists.
- The Doctor RPC trusted the client to supply a bounded context.
- The runner-tarball resolve hit the GitHub API under the same unbounded
  state context.

Tellingly, `tools/seedpull` wrapped the identical `Resolve` call in a
1-minute timeout — the discipline held in one caller and not the other
three. That asymmetry is the argument: conventions enforced by review do
not survive call-site growth.

## Decision

`internal/bounded` owns the invariant. `bounded.Context` is a
`context.Context` whose inner context is **unexported** — it cannot be
constructed outside the package, so it can only come from a constructor
that attaches a bound:

- `bounded.WithTimeout` / `bounded.WithDeadline` — wall-clock bounds.
- `bounded.Stall.Watch` — the progress bound (moved here from `internal/oci`;
  it is not pull-specific, it is the second legitimate way to bound an
  operation whose duration is unknowable up front).

Functions that perform network or guest I/O take `bounded.Context` instead
of `context.Context`. An unbounded call site is then a **compile error**:
the author is forced to decide, at the call site, what the bound is — which
is exactly the decision the four bugs above skipped. Because the inner field
is unexported, `bounded.Context{someCtx}` does not compile either; the type
cannot be laundered.

All guest/network seams are converted: `oci.Client.Resolve`/`PullTo`, the
`images.RunnerResolver` seam, `github.Client`'s five entry points,
`sshx.Dial`/`WaitFor`/`Output`, `guest.Dialer.WaitFor`/`PullDiag`, and
`vm.Manager.Boot`/`Machine.WaitIP`/`Machine.Stop`. The FSM's `enter` hands
each deadline-bounded state a `bounded.Context` directly, so the per-state
deadline reaches the seams through the type system.

Three functions deliberately keep plain `context.Context` — converting them
would brand a lifetime handle as an operation bound:

- `sshx.Client.Start` / `Guest.StartRunner`: the ctx is the runner proc's
  *lifetime* (run.sh must outlive PROVISION's deadline); establishment is
  bounded internally by the socket-deadline recipe.
- `ImageEnsurer.Ensure`: ENSURE_IMAGE has no wall-clock deadline because
  pull duration is unknowable; its operations bound themselves internally
  (resolve timeout, stall watcher).

### The delegated bound

A third legitimate bound exists alongside wall-clock and stall: **waiting on
a peer's bounded operation**. When slots serialize on a per-destination lock
(the image-pull and runner-tarball locks), the waiter's wait ends exactly
when the holder's own stall-watched transfer does — the bound is delegated,
not absent. Two conditions make the delegation honest rather than a loophole:

1. **The wait must be interruptible** — a `select` on the lock and
   `ctx.Done()`, never a bare mutex acquire, so an operator recycle or
   shutdown still ends it early.
2. **The wait must be visible** — the waiter annotates ("waiting for a
   concurrent …") and keeps its own stall detector fed, because to a watcher
   that only counts the waiter's bytes, a healthy delegated wait is
   indistinguishable from a dead transfer; left unfed it reported STALLED and
   killed the context the waiter would need if the holder failed.

## Rejected alternatives

- **A nogo analyzer** flagging calls whose context lacks a provable bound:
  static analysis cannot in general prove a `context.Context` value carries a
  deadline (it flows through interfaces, struct fields, and function
  boundaries), so the check is either unsound (misses the FSM's `d=0`
  no-deadline state path — the actual bug) or so noisy it gets suppressed.
  The type system performs the same check soundly and locally. A narrow nogo
  rule may still come later for the complementary invariant (no `http.Client`
  construction outside blessed packages).
- **Runtime check at entry points** (`if _, ok := ctx.Deadline(); !ok
  { return err }`): catches the bug in production instead of at compile
  time, and cannot accept stall-bounded contexts, which carry no deadline —
  the dominant bound for pulls.
- **Keep the discipline, add review checklists**: that was the status quo;
  it produced four sites in 6.7k lines.
