# ADR-0005: Bazel as the unified build engine

**Status:** Accepted (2026-06-09)

## Context

The monorepo spans Go (daemon + CLI), Swift (menu-bar app), and a protobuf
contract consumed by both. Two orchestration shapes were evaluated:

1. **Task-runner orchestration**: `just` recipes calling `go build`,
   `xcodebuild` (via Tuist for declarative project generation), and `buf
   generate` for committed cross-language protocol types.
2. **Unified build graph**: Bazel 9 (bzlmod) with rules_go + rules_swift +
   rules_apple, protocol types generated in-graph.

## Decision

Bazel 9, bootstrapped from the
[bazel-starters/go](https://github.com/bazel-starters/go) template: one
hermetic graph, one entry point (`bazel build //...` / `bazel test //...`),
in-graph proto generation (no committed generated code — nothing to drift),
and rule-managed codesigning/entitlements for the macOS targets. The
rules-maintenance tax (version churn across rules_go/rules_swift/rules_apple)
is accepted.

Template adoption is selective: bzlmod layout, gazelle, nogo, the
`format_multirun` formatter, and `tools/platforms` are kept; the
Aspect-CLI-coupled machinery (multitool, bazel_env, orion gazelle extension,
devcontainer, OCI/distroless image rules) is dropped — runny ships binaries
and an .app, not containers, and the extra indirection wasn't carrying weight
for a solo repo.

## Rejected alternatives

- **just + buf + Tuist**: simpler day-1 setup and better Swift IDE defaults,
  but two uncoordinated build worlds, committed generated code requiring an
  equality gate, and a third tool (Tuist) solely to avoid hand-maintaining an
  Xcode project that ADR-0007 eliminates anyway.

## Consequences

- Dependencies are managed through the graph; the exact workflow (with a
  load-bearing flag and step order) lives in CONTRIBUTING.md.
- Swift/Apple targets build only on macOS hosts; the build graph must keep
  pure-Go packages testable on Linux (dev box) via build constraints.
