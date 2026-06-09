# CLAUDE.md — AI Agent Index

> **Keep this file lean.** It is the project-wide pointer index for agents. Detailed docs live under `docs/`; the human dev workflow lives in `CONTRIBUTING.md`; the rules that govern the doc system live in `docs/documentation-system.md`. When you change a feature, update its architecture doc or the relevant directory `CLAUDE.md`, not this index.

## Project

**runny** — an observable macOS GitHub Actions runner daemon: crash-only ephemeral runner VMs on Virtualization.framework, fully compatible with tart's bundle/OCI image format but with no tart binary at runtime. Three artifacts: `runnyd` (Go daemon), `runnyctl` (Go CLI), `RunnyBar` (SwiftUI menu-bar app). See `docs/architecture/` for the shape and `docs/architecture-decisions/` for the decisions behind it.

## Tech stack

Go 1.26 (mise-managed) for daemon + CLI; cgo to Virtualization.framework via `Code-Hex/vz`; SSH via `x/crypto/ssh` only (ADR-0002). Swift/SwiftUI for the app only (ADR-0001). Build: Bazel 9 / bzlmod with gazelle-managed BUILD files (ADR-0005); the `runny.v1` protobuf contract is generated in-graph, never committed (ADR-0006). The full dependency set lives in `go.mod` and `MODULE.bazel` and is not re-listed here.

## Commands

`bazel build //...` · `bazel test //...` · `bazel run //tools/format` · `bazel run //:gazelle` (after changing Go imports). Dependency workflow: `go mod tidy` → `bazel mod tidy` → `bazel run //:gazelle`. Full reference and dev setup: `CONTRIBUTING.md`.

**Darwin-only targets** (vz cgo, RunnyBar) do not build on Linux — develop here, build/test them on ix (`tools/ix` sync helper; see CONTRIBUTING.md).

## Source tree

- `cmd/runnyd`, `cmd/runnyctl` — binaries; thin mains over `internal/`.
- `internal/statemachine` — the 11-state crash-only FSM (ADR-0004); per-state deadlines, backoff, cycle.json.
- `internal/vm` — tart-bundle parsing + Virtualization.framework lifecycle via vz (darwin-tagged; ADR-0008).
- `internal/oci` — tart-format image pull (non-standard OCI layout, LZ4 layers).
- `internal/sshx` — the only package allowed to construct SSH clients (deadline recipe, ADR-0002).
- `internal/github` — App JWT → installation token → JIT config; runner list/delete (ADR-0003).
- `internal/home` — the `~/.runny` on-disk layout: images, vms, cycles, logs, socket.
- `internal/socket` — the gRPC server over the unix socket.
- `proto/runny/v1` — the contract `runnyctl` and RunnyBar both consume.
- `apps/RunnyBar` — SwiftUI MenuBarExtra (ADR-0007: no .xcodeproj, ever).
- `tools/` — format runner, nogo, platforms.

For per-directory detail, read that directory's `CLAUDE.md` if present. For current counts and inventories (states, RPCs, config keys), read the source (the FSM table, the proto file, the config schema); this index does not enumerate them.

## Documentation map

| Topic | Home |
| --- | --- |
| How it works (current architecture) | `docs/architecture/` |
| Decisions, and why | `docs/architecture-decisions/` |
| Design plans (multi-phase work) | `docs/design-plans/` |
| Doc-system governance | `docs/documentation-system.md` |
| Human dev workflow | `CONTRIBUTING.md` |

## Invariants

- **No unbounded operations.** Every guest-facing call carries a deadline; SSH clients come from `internal/sshx` only — plain `ssh.Dial` silently reintroduces the failure mode this project exists to kill (ADR-0002).
- **Crash-only.** Failure handling is destroy-and-recycle, never repair-in-place; teardown cannot fail (ADR-0004).
- **No tart binary at runtime**; tart *format* compatibility is the contract (ADR-0008).
- **No .xcodeproj, ever.** Xcode is SDK-vendor only (ADR-0007).
- **Conventional Commits**, enforced by the commit-msg hook; atomic commits. See `CONTRIBUTING.md`.
- **Three-tier hooks** — pre-commit (format), commit-msg (commitlint), pre-push (build + test); CI re-runs them. Don't bypass.
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
