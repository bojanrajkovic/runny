# ADR-0001: Go for the daemon and CLI; Swift for the menu-bar app only

**Status:** Accepted (2026-06-07, reconfirmed 2026-06-09)

## Context

runny replaces khoi/sand, a Swift daemon whose failure modes (unbounded SSH via
sshpass, blocking pipe reads starving the cooperative thread pool) caused a
10-week silent runner outage. The daemon's core competency is **bounded SSH**
into freshly-booted guests: every connect, banner exchange, auth, and exec must
carry a hard deadline.

An all-Swift repo was attractive (shared models between daemon and app, no
cross-language contract), so swift-nio-ssh was evaluated as the SSH layer on
2026-06-09.

## Decision

The daemon (`runnyd`) and CLI (`runnyctl`) are Go. Swift appears only in the
`RunnyBar` menu-bar app, which talks to the daemon over the protobuf socket
contract (ADR-0006) like any other client.

## Rejected alternatives

**Swift daemon on swift-nio-ssh** (v0.13.0, Apr 2025; actively maintained but
0.x):

- No native deadline support for TCP connect, banner exchange, or auth — each
  requires hand-composed NIO primitives (`IdleStateHandler` layering, scheduled
  promise-failing). Go's recipe is two lines (ADR-0002), spike-proven.
- [swift-nio-ssh#86](https://github.com/apple/swift-nio-ssh/issues/86) — client
  stuck indefinitely pre-handshake, open ~5 years. That is sand's outage
  failure mode as a known open bug in the candidate library.
- Half-closure is broken by default (README warns child channels "behave
  extremely unexpectedly" without explicit opt-in) — the pipe-buffer territory
  this rewrite escapes.
- Glue estimate 300–500 lines vs ~50 in Go; the Citadel wrapper (also 0.x)
  improves ergonomics but adds no deadlines.

## Consequences

- The daemon↔app boundary is a wire contract, not shared code (ADR-0006).
- Go's cgo bridges Virtualization.framework (ADR-0008); daemon builds are
  macOS-only, pure-Go packages test anywhere.
