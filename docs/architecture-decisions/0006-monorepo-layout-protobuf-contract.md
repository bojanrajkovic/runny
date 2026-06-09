# ADR-0006: Monorepo layout and the in-graph protobuf contract

**Status:** Accepted (2026-06-09)

## Context

Three artifacts (Go daemon, Go CLI, Swift app) share exactly one thing: the
control protocol over the daemon's unix socket. The repo layout should make
language toolchains own their subtrees natively, with one deliberate shared
surface.

## Decision

```mermaid
flowchart LR
    subgraph host["macOS host"]
        runnyd["runnyd (Go)<br/>state machines + vz"]
        sock[("unix socket<br/>~/.runny/runnyd.sock")]
        vm1["macOS guest slot 1"]
        vm2["macOS guest slot 2"]
        runnyd -- "Virtualization.framework (in-process)" --> vm1
        runnyd --> vm2
        runnyd --- sock
    end
    ctl["runnyctl (Go CLI)"] -- "protobuf (runny.v1)" --> sock
    bar["RunnyBar (SwiftUI)"] -- "protobuf (runny.v1)" --> sock
    runnyd --> gh["GitHub API"]
    runnyd --> ghcr["ghcr.io images"]
```

- `cmd/runnyd`, `cmd/runnyctl` — Go binaries (gazelle-managed).
- `internal/` — daemon libraries; one package per concern (statemachine, sshx,
  vm, oci, github, home, socket).
- `proto/runny/v1/` — the contract: `proto_library` → `go_proto_library` +
  `swift_proto_library`, generated **in-graph**. No committed generated code.
- `apps/RunnyBar/` — SwiftUI app (ADR-0007).
- `docs/` — architecture, this ADR series, design plans, doc-system governance.

**`runnyctl` and RunnyBar are deliberately symmetric**: sibling clients of the
same `runny.v1` contract, no privileged path for either. Anything the CLI can
do, the app can do, by construction.

## Rejected alternatives

- **Shared Go model package imported by the CLI, ad-hoc JSON for the app**: a
  privileged client and a hand-maintained second serialization that drifts.
- **Committed buf-generated code**: works without Bazel, but requires a
  byte-equality gate to keep honest; in-graph generation makes drift
  structurally impossible.
