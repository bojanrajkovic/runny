# CLAUDE.md — AI Agent Index

> **Keep this file lean.** It is the project-wide pointer index for agents. Detailed docs live under `docs/`; the human dev workflow lives in `CONTRIBUTING.md`; the rules that govern the doc system live in `docs/documentation-system.md`. When you change a feature, update its architecture doc or the relevant directory `CLAUDE.md`, not this index.

## Project

**runny** — an observable macOS GitHub Actions runner daemon: ephemeral, destroy-on-failure runner VMs on Virtualization.framework, fully compatible with tart's bundle/OCI image format but with no tart binary at runtime. Three artifacts: `runnyd` (Go daemon), `runnyctl` (Go CLI), `Runny` (SwiftUI app: menu bar + main window). See `docs/architecture/` for the shape and `docs/architecture-decisions/` for the decisions behind it.

## Tech stack

Go 1.26 (mise-managed) for daemon + CLI; cgo to Virtualization.framework via `Code-Hex/vz`; SSH via `x/crypto/ssh` only (ADR-0002). Swift/SwiftUI for the app only (ADR-0001). Build: Bazel 9 / bzlmod with gazelle-managed BUILD files (ADR-0005); the `runny.v1` protobuf contract is generated in-graph, never committed (ADR-0006). The full dependency set lives in `go.mod` and `MODULE.bazel` and is not re-listed here.

## Commands

`bazel build //...` · `bazel test //...` · `bazel run //tools/format` · `bazel run //:gazelle` (after changing Go imports). Dependency changes follow CONTRIBUTING.md's workflow exactly — its flag and step order are load-bearing. Full reference and dev setup: `CONTRIBUTING.md`.

**Darwin-only targets** (vz cgo, the Runny app) build and test only on Darwin. On other hosts, everything pure-Go still builds and tests; for the cross-host loop see CONTRIBUTING.md.

## Source tree

- `cmd/runnyd`, `cmd/runnyctl` — binaries; thin mains over `internal/`.
- `internal/bounded` — `bounded.Context`: the no-unbounded-operations invariant as a type (ADR-0011); wall-clock and progress-stall bounds.
- `internal/obs` — the structured observability event stream (ADR-0024): `Event`/`Kind`, the context-carried scope, `Action(ctx, name, fn)`, and `HTTPTransport` (the RoundTripper every egress client wears); the seam every OTLP/actions-artifact consumer builds on, with no telemetry SDK imports.
- `internal/telemetry` — runny's only OTEL importer (ADR-0024): installs OTLP trace + metric providers from `observability.otlp` config, resource attribution, bounded shutdown; installs nothing when the config block is absent. Also the trace and metrics emitters, the one place either kind of event ever gets folded: the trace side renders a cycle's `obs.Event`s as a `runny.cycle` → `cycle.step` → `cycle.step.action` span tree and a shared image pull's as its own flat `runny.pull` root with `http <class>` children (SDK-random span IDs; the two subtrees correlate by attribute — `runny.cycle_id`/`runny.pull.id` — not by parentage, since a pull belongs to no single cycle); the metrics side folds the same stream into cycle/step/job/action instruments plus the pull/tarball-download ones (no second injected seam), and polls slot statuses into observable gauges.
- `internal/statemachine` — the destroy-and-recycle FSM (ADR-0004); per-state deadlines, backoff, cycle.json.
- `internal/clonefile` — the APFS clonefile(2) wrapper: single-file copy-on-write clone (darwin-tagged), used by both the tart bundle clone and the per-cycle runner-tarball clone.
- `internal/tart` — the tart bundle format: config.json parsing, validation, bundle clone (delegates per file to `internal/clonefile`).
- `internal/vm` — Virtualization.framework lifecycle via vz (darwin-tagged; ADR-0008) + guest sizing.
- `internal/oci` — tart-format image pull (non-standard OCI layout, LZ4 layers).
- `internal/images` — the ENSURE_IMAGE ensurer: image + runner-tarball caching (the tarball download store, cold-start pruned; each cycle clones its own copy before boot), stall watching, pull progress; the shared image-puller actor that lets concurrent slots share one pull and its outcome, incl. a bounded hold on a deterministic disk-headroom failure (ADR-0021). Emits ENSURE_IMAGE action events via `obs.Action`, and the shared puller emits its own pull-scoped events (`obs.WithPull`) — no injected metrics seam, no OTEL imports. Also the on-demand disk reclaim planner used by `runnyctl prune` (see `prune.go`).
- `internal/sshx` — the only package allowed to construct SSH clients (deadline recipe, ADR-0002).
- `internal/guest` — what to do over SSH: provision scripts, runner launch, diag pull.
- `internal/github` — App JWT → installation token → JIT config; runner list/delete (ADR-0003).
- `internal/home` — the `~/.runny` on-disk layout (images, vms, cycles, logs, socket, instance-id) + config schema and runner-name rules.
- `internal/cycle` — per-cycle artifact records (cycle.json) and retention.
- `internal/logring` — log fan-out: file sink + in-memory rings (daemon log, runner output) behind StreamLogs.
- `internal/socket` — the gRPC server over the unix socket.
- `proto/runny/v1` — the contract `runnyctl` and Runny both consume.
- `apps/Runny` — SwiftUI app: MenuBarExtra popover + main window (ADR-0016; ADR-0007: no .xcodeproj, ever).
- `tools/` — format runner, nogo, platforms, the `config.yaml` JSON Schema generator (`configschema`).

For per-directory detail, read that directory's `CLAUDE.md` if present. For current counts and inventories (states, RPCs, config keys), read the source (the FSM table, the proto file, the config schema); this index does not enumerate them.

## Documentation map

| Topic | Home |
| --- | --- |
| How it works (current architecture) | `docs/architecture/` |
| Decisions, and why | `docs/architecture-decisions/` |
| Security posture | `docs/security.md` |
| Installing/operating runnyd on a host | `docs/deploy.md` |
| Doc-system governance | `docs/documentation-system.md` |
| Human dev workflow | `CONTRIBUTING.md` |

## Invariants

- **No unbounded operations.** Every guest-facing call carries a deadline or a progress bound, enforced by the type system: guest/network seams take `bounded.Context`, constructible only with a bound attached (ADR-0011; the ADR lists the few deliberate lifetime-context exceptions). SSH clients come from `internal/sshx` only — plain `ssh.Dial` silently reintroduces the failure mode this project exists to kill (ADR-0002).
- **No silent failure.** A broken guest is met with a visible, recorded destroy-and-recycle, never in-place repair; teardown can't silently stick; a restart is a cold start. Defined in `docs/architecture/no-silent-failure.md` — including what it is *not* (distinct from ephemeral/hermetic guests and from no-unbounded-operations); the FSM that enforces it is ADR-0004.
- **The app is non-privileged.** The Runny app owns only the user's `gui/` launchd domain and raises no `with administrator privileges` prompt, ever; `system/`, root, and installing `runnyctl` to a system path are runnyctl's. Defined in `docs/architecture/privilege-boundary.md` (ADR-0023). It still *observes* a system daemon read-only (the unprivileged ownership probe); it never *manages* one.
- **No tart binary at runtime**; tart *format* compatibility is the contract (ADR-0008).
- **No .xcodeproj, ever.** Xcode is SDK-vendor only (ADR-0007).
- **Conventional Commits**, enforced by the commit-msg hook; atomic commits. See `CONTRIBUTING.md`.
- **Three-tier hooks** — pre-commit (format + zizmor on workflows), commit-msg (commitlint), pre-push (build + test); CI re-runs them. Don't bypass.
- **`AGENTS.md` symlinks** — every `CLAUDE.md` has a sibling `AGENTS.md` symlink.
- **Reference content is read from source, never enumerated in prose.** See `docs/documentation-system.md`.

## Planning and design

When planning, building, and shipping a change here:

0. **Start from current `main`, and re-sync before you push.** `git fetch` and rebase before grounding yourself; check mergeability again right before opening the PR and re-verify on the new base if `main` moved.
1. **Ground yourself in the docs and code, and verify before you assert.** Read the relevant directory `CLAUDE.md`, the architecture docs, the ADRs, and the actual source before proposing. Grep or read to confirm a claim before you build on it.
2. **Gather the task's context up front** — what is already true, the constraints, the prior decisions — instead of rediscovering them mid-build.
3. **Ask clarifying questions freely, and name your assumptions out loud.** A thorough `AskUserQuestion` pass beats a confident wrong guess.
4. **Pin "done" and "out of scope" before designing.** Deliverable, success criteria, and what you are explicitly *not* doing.
5. **Brainstorm two or three alternatives; don't ship the first idea.** Prefer the smallest change that fits existing patterns, *and* invest in the right abstraction when the repetition is demonstrated rather than speculative — copy first, abstract on the third.
6. **Be adversarial: attack your own proposal.** Hunt the failure mode — here that especially means the hung guest, the half-dead VM, the GitHub API blip, the deadline that doesn't fire.
7. **Record decisions with real alternatives as ADRs, at decision time.** See `docs/documentation-system.md` for what is ADR-worthy.
8. **Update the docs as part of the change, not after.** The affected architecture doc, directory `CLAUDE.md` sharp edges, and ADRs are part of "done".
9. **Know what runny optimizes for: silent-failure-proofness over throughput.** When a trade-off pits performance or elegance against "can this hang or fail silently", the latter always wins — that is the lesson the predecessor's 10-week outage paid for.
10. **Before you ship, review the diff with `/code-review`, scaled to the change** — `low`/`medium` for localized changes, `high` for cross-cutting ones, `max` for anything in the state machine, vm, or sshx core.
