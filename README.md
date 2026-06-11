# runny 🏃

An observable macOS GitHub Actions runner daemon: crash-only ephemeral runner
VMs on Apple's Virtualization.framework, fully compatible with
[tart](https://github.com/cirruslabs/tart)'s bundle and OCI image format — with
no tart binary at runtime.

Three artifacts, one contract:

- **`runnyd`** — the daemon. One deadline-bounded state machine per runner
  slot: pull image → clonefile → boot (in-process via
  [vz](https://github.com/Code-Hex/vz)) → SSH provision → JIT-register →
  listen → run one job → destroy → repeat. Every failure converges to
  destroy-and-recycle with capped backoff; every cycle writes a
  machine-readable post-mortem.
- **`runnyctl`** — the CLI over a unix socket: live status and runner logs,
  recycle/pause, per-cycle post-mortems (`why`), environment checks
  (`doctor`).
- **`RunnyBar`** — a SwiftUI menu-bar app, a sibling client of the same
  protobuf contract.

Built because the predecessor converted every transient failure into a
permanent silent outage. runny's design rule: **no operation is ever
unbounded, and no failure is ever silent.**

## Status

Pre-1.0, under active construction. See `docs/architecture-decisions/` for the
decision record and the GitHub issues for what's in flight.

## Building

```
mise install
bazel build //...
```

Pure-Go packages build and test anywhere; the daemon binary and app require a
macOS arm64 host (see `CONTRIBUTING.md` for the dev loop and codesigning).

## License

MIT — see [LICENSE](LICENSE).
