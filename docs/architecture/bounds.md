# Bounds: timeouts are backstops, not targets

The no-unbounded-operations invariant says *every* guest- or network-facing wait
carries a bound. This doc is the sizing rule for those bounds — the principle
that governs what number (or what progress signal) you attach. The enforcement
mechanism (the `bounded.Context` type, and the three kinds of bound it admits)
lives in
[ADR-0011](../architecture-decisions/0011-bounded-contexts.md); this is the law
that decides how each one is sized.

## The principle

A timeout is a **backstop against a transient latency spike — not a target, and
not an expected duration.** Size every bound to the *healthy* magnitude of the
operation it guards, with generous flat margin, and let it fire when exceeded:
**exceeding the bound is the degraded condition the backstop exists to catch.**

The corollary that follows: a bound that is routinely approached in healthy
operation is mis-sized. If you find yourself raising a timeout because it
"sometimes" fires under load, stop — either the operation is genuinely degraded
(and firing is correct), or the bound was sized to the wrong magnitude.

## Match the bound to the operation's kind

The three legitimate bound kinds (ADR-0011) map onto three operation magnitudes.
Picking the right kind *is* picking the right magnitude:

- **Control-plane / metadata operations** — an RPC handshake, a GitHub API call,
  an image-manifest resolve, a daemon's startup checks. These are **O(seconds)**
  when healthy and take a **seconds-scale wall-clock** backstop. A few seconds is
  the magnitude; tens of seconds is already a generous backstop.
- **Data-plane transfers** — the image-layer pull. These are **O(minutes)** to
  **O(hours)** and their duration is unknowable up front, so they are
  **progress-bounded** (a stall watcher on bytes moved), **never**
  wall-clock-capped. A wall-clock cap on a transfer is wrong in both directions
  at once: too short and it kills a healthy slow pull, too long and it is useless
  as a backstop — the two failure modes a magnitude-mismatched bound always
  produces.
- **Job execution** — unbounded by design. The daemon refuses a drain budget on
  purpose: the only way a budget could fire is by killing a running job, the
  failure this project exists to avoid.

A wait *on* one of these (a peer serialized behind another slot's pull) is the
**delegated** bound: it ends exactly when the operation it waits on ends. The
magnitude is inherited, not re-chosen.

## The canonical example: ENSURE_IMAGE

One FSM state holds both magnitudes back to back, which is why it is the
clearest illustration:

- The manifest **resolve** is metadata — a registry round-trip that returns a
  digest. Healthy: a second or two. It carries a **seconds-scale wall-clock**
  budget (`Deadlines.Resolve`).
- The layer **pull** that follows is a transfer — gigabytes over the network.
  Healthy: minutes. It carries a **byte-progress stall** budget, and the state
  itself is entered with *no* wall-clock deadline.

Wall-clock-capping the pull at the resolve's magnitude would strangle every real
image; sizing the resolve at the pull's magnitude would let a dead registry hang
a slot for minutes before the backstop fired. Same state, same code path, two
deliberately different bounds — because they guard two different magnitudes.

## The forbidden move, stated two ways

**Never size a downstream wait to the *sum* of upstream backstop budgets. And
never derive a wait from the daemon's own degraded-budget envelope** (a
per-check budget times the number of checks). They are the same error: both bake
"every upstream operation is simultaneously degraded to its failure threshold"
in as the baseline.

A daemon whose startup checks are each burning their full backstop is *not
coming up healthy*. A check sitting at its 30-second backstop is the **degraded**
case the backstop catches — it is not a healthy duration to be accommodated by
inflating the downstream wait. Sizing a client's respawn wait to a flat
cold-start magnitude (a healthy cold start is O(seconds), so a flat ~90s is wide
margin) and **letting it fire** is the correct, fast verdict. Sizing it to
`per-check-budget × number-of-checks` would hide that failure behind a
multi-minute horizon — in exactly the degraded scenario where the operator needs
to be told promptly. The honest input is always *healthy magnitude × margin*; a
per-check backstop is never one.

## This applies to client UX as rigorously as to daemon internals

The principle is not a daemon-only concern. `runnyctl reload -wait` and the Runny
app are bounded by magnitude-matched backstops, never by a sum of the daemon's
internal budgets: the reload preflight RPC carries a flat backstop over a
healthy-slow preflight; the respawn wait a flat backstop over a healthy cold
start; the live drain a progress bound on `drain_seq`, not a wall clock. See
[reload-convergence.md](reload-convergence.md) for how those three clocks are
sized and why none is a sum.
