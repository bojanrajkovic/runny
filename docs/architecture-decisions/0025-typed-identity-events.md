# ADR-0025: Typed events carry mid-cycle identity; action attributes stay action-local

**Status:** Accepted (2026-07-02)

## Context

The observability event stream ([ADR-0024](0024-observability-event-seam.md))
moves identity a cycle learns mid-flight — a VM's MAC and IP, the GitHub
runner ID, the resolved image digest — from the FSM to consumers. Two
mechanisms grew side by side:

- **Dedicated typed events** (`vm_info`, `runner_info`): a `Kind`, a payload
  struct, a union field on `Event`, and a handler in each consumer.
- **Attributes on actions** (`obs.Attr`): free string key/value pairs passed
  to `obs.Action` at call time, forwarded verbatim to the action's span.

Instrumenting the image ensurer forces the choice: the resolved digest and
the runner-tarball version are learned inside ENSURE_IMAGE's sub-steps, and
the shared pull actor needs a correlation id on each cycle's wait. Letting
both mechanisms keep growing undecided would fork the schema — each new fact
would re-ask the question.

What distinguishes the two:

- **Typed payloads.** Go has no arithmetic type parameters or union types to
  make a string-keyed attribute bag safe; an `int64` runner ID in a typed
  struct stays an `int64`, while an attribute would be a string a consumer
  parses back.
- **Consumer routing differs per fact.** The digest lands on the cycle root
  *and* the owning step span; audit detail becomes span events; a runner ID
  annotates only its step. A typed event's handler encodes that routing; an
  attribute mechanism would need a routing table keyed by attribute name —
  the same code, hidden in string matching.
- **Replay.** Retained cycle records determine the event stream for re-emit
  tooling; a typed event maps to record fields by construction.
- **Cost.** A typed event costs a Kind + struct + union field + consumer
  handlers. An attribute costs one constant.

## Decision

**Identity a cycle learns travels as typed events.** `image_info` joins
`vm_info`/`runner_info`, emitted by the FSM at the same sites the record
learns the fact (one code path, two outputs). Consumers route each fact
explicitly.

**Action attributes are reserved for action-local facts**: values that
describe *that execution of that action* and die with its span — the SSH
hardening mode of a rotate, the pull id correlating a cycle's wait with the
shared pull that served it. They never carry facts a consumer must lift
elsewhere (root, step, metrics), and their keys and values stay closed sets.

## Alternatives considered

**Attributes as the standard transport** (extend `obs.Action` to accept
result attributes, migrate `runner_info`): one mechanism instead of two, no
new Kinds. Rejected: it trades the visible cost (a struct per fact) for a
hidden one (a stringly-typed routing table per consumer), loses typed
payloads, and would still need the up-front-attrs form for span-local facts —
two mechanisms again, just less honest ones.

**Both, chosen per fact by the author**: rejected outright; that is the
undecided state this ADR exists to end.

## Consequences

- Adding a liftable fact means adding a Kind, a payload struct, and consumer
  handlers — deliberate friction that keeps the schema reviewed.
- `obs.Attr` stays small and closed; a reviewer seeing a new attribute key
  asks only "is this action-local?".
- No migration: `vm_info`/`runner_info` were already on the winning side.
