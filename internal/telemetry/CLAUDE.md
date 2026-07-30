# internal/telemetry — AI Agent Notes

Owns the OTel pipeline (`telemetry.go`), the metric instruments
(`metrics.go`), and the span assembler (`trace.go`). This doc is sharp edges
only; the assembler's own doc comments carry the per-span detail.

## Sharp edges

- **Traces are assembled from the event stream, not from spans held in a
  caller's context.** `NewTraceConsumer` returns an `obs.Emitter`; every span
  runny exports is created by `traceAssembler` in response to an `obs.Event`,
  and the span handles live in the assembler's own maps. `runny.cycle` starts
  from `context.Background()` by construction. **No `context.Context` reaching
  domain code ever carries a live span**, so `tracer.Start(ctx, …)` written
  anywhere outside this package produces a root, not a child. Instrument by
  emitting an `obs` event; that is the only seam that parents correctly.
- **A dependency that instruments itself must be ADAPTED into `obs`, never
  bridged straight to OTel.** This is the same rule seen from the outside. A
  bridge converts the library's spans into OTel spans directly, and those need
  an OTel parent in the caller's context — which, per the point above, never
  exists here. `opencensus_bridge_windows.go` did exactly that at first and
  every vendored `winhcs` span became its own root: roughly six disconnected
  traces per cycle, each one a real HCS operation rendered as though it
  belonged to nothing. It now implements `octrace.Tracer` and emits
  `KindBackendStarted`/`KindBackendEnded` instead. Anything else with built-in
  tracing wants the same treatment, and the adapter is the worked example.
- **`KindBackendStarted`/`KindBackendEnded` are paired because backend spans
  NEST; `KindHTTP` is a single completion event because round trips do not.**
  Spans end innermost-first, so a lone end event would describe a child before
  its parent existed. Don't "simplify" the backend pair into the HTTP shape —
  the nesting (a syscall-level span inside its wrapper) is where a
  slow-but-not-wedged host shows up, since the gap between the two levels is
  runny's own bounded wait.
- **A backend span still open when its cycle finishes is closed there and
  marked `runny.backend.unfinished`.** `Boot` bounds its own wait around the
  vendored create/start, so a slow host leaves a detached goroutine running
  after the cycle is gone. Two failure modes are being avoided at once: an
  unended span is never exported *at all*, so the trace of the cycle that
  failed on that very call would show no sign of it; and a span closed
  silently at cycle end reads as a call that returned promptly. The real end
  time is unknowable from here and is deliberately not guessed — the late end
  event finds the cycle gone and no-ops.
- **`spanHandle.ctx` is kept so later children can parent under it**, which is
  why handles are retained for open actions (`ss.current`) and open backend
  spans rather than just their `trace.Span`. Dropping the ctx would flatten
  everything that arrives while they are open.
