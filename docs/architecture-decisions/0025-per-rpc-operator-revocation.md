# ADR-0025: Per-RPC operator revocation enforcement

**Status:** Accepted (2026-07-03)

## Context

Operator ACL membership ([ADR-0014](0014-debug-key-injection.md)'s grant/revoke
extension) was enforced only at `connect()` time: `net.Listen`'s socket
permissions and the ACL gate a new dial, but a gRPC connection, once
established, carries no further per-RPC identity recheck. A revoked operator
holding an already-open connection could keep invoking every RPC — including
all nine mutating ones — until the connection happened to close. `runnyctl
operator revoke` visibly removed the ACE, but gave no guarantee the revoked
party's session actually stopped acting. This is the documented lingering-
connection gap ([runny#221](https://github.com/bojanrajkovic/runny/issues/221)).

## Decision

**A server-wide gRPC interceptor pair (unary + stream) rechecks every RPC's
peer uid against a fresh ACL read at RPC-start, uniformly across all RPCs —
no per-method allowlist.** The kernel-authenticated peer uid is already
carried in every RPC's context (`peerUID`, stamped once per connection by
`peerCreds.ServerHandshake` via `SO_PEERCRED`), so no new identity plumbing
is needed. Root (uid 0) always passes; an unreadable peer uid is denied
(fail closed). The gate arms only on the system daemon — a per-user home has
no ACL-managed operator set to enforce against.

**Revoked operators' in-flight streams are actively killed, not merely
left to expire.** The stream interceptor wraps each gated stream in a
cancellable child context and registers `(uid → cancel)` in a registry on
the gate, **before** running its ACL check. `revokeOperator`, after its ACL
mutation lands (inside the existing `operatorMu` critical section), sweeps
the registry and cancels every stream owned by the revoked uid. Both
existing stream handlers (`WatchStatus`, `StreamLogs`) already `select` on
`stream.Context().Done()`, so cancellation ends them with zero handler
changes; the interceptor converts that cancellation into a typed
`PermissionDenied` instead of the handler's plain `nil`, so the client sees
"access revoked," not a silent clean close.

Registering before checking is what closes the open-vs-revoke race: a stream
opening concurrently with a revoke either fails the (post-mutation) ACL
check, or is already in the registry when the sweep runs. No interleaving
lets a stream slip through unrevoked.

## Rejected alternatives

- **Gating only the mutating RPCs via a method allowlist.** Relocates the
  original per-RPC drift from handlers into an interceptor list instead of
  eliminating it — every future RPC defaults to *ungated* unless someone
  remembers to add it. Uniform gating is default-deny for anything new, and
  denying reads post-revoke matches what a fresh `connect()` would already
  do.

- **Per-`SendMsg` ACL recheck on every stream message.** Catches out-of-band
  ACL edits too (not just the in-process revoke path), but puts a cgo
  `acl_get_file` call on the one genuinely bursty path in the daemon
  (`StreamLogs -follow` at log-line rate). Rejected for the steady-state
  cost.

- **A per-stream polling goroutine rechecking every N seconds.** Adds
  polling lag (a revoke isn't instant) and a goroutine per open stream, for
  no better guarantee than the event-driven registry gives for free.

- **Per-handler rechecks scattered across each RPC method.** The
  ADR-0014-era (debug-key injection) failure mode this whole change closes: identical logic
  duplicated per handler, one miss away from silently exempting a new RPC.

## Consequences

- Every RPC — read or write — now pays one uncached `opacl.ListUIDs` call
  (uid comparison only, no `user.LookupId` username resolution) when the
  gate is armed. No caching layer exists; add one only if a profile ever
  demonstrates this as a hot path (none does — streams are checked once at
  open, not per message).
- `opacl.List` is now a thin wrapper over the new `opacl.ListUIDs`, which the
  gate calls directly to avoid paying NSS username-resolution latency on
  every RPC.
- Out-of-band ACL edits (a manual `chmod`, or `install-daemon` resetting the
  ACL) are not swept for already-open streams — only the in-process
  `RevokeOperator` path triggers `killStreams`. Every *new* RPC still
  observes the edit immediately via the fresh `ListUIDs` read. Accepted:
  those paths never terminated live connections before this change either.
- `docs/security.md`'s operator section is rewritten: the "read stream may
  linger" allowance is superseded — revocation now takes effect at the next
  RPC on any connection, and the daemon-performed revoke terminates the
  revoked operator's live streams.
