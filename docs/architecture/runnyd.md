# runnyd architecture

The current shape of the daemon. Decisions and their alternatives live in
`../architecture-decisions/`; this doc tracks the code.

## System shape

The diagram lives in [ADR-0006](../architecture-decisions/0006-monorepo-layout-protobuf-contract.md).
In one paragraph: `runnyd` runs one crash-only state machine per runner slot,
boots tart-format macOS guests in-process via Virtualization.framework
(`internal/vm`), provisions them over deadline-bounded SSH (`internal/sshx` →
`internal/guest`), registers them with GitHub via JIT config
(`internal/github`), and serves the `runny.v1` control surface over a unix
socket (`internal/socket`) to `runnyctl` and `RunnyBar` as equal clients.

## The cycle

The FSM (states, transitions, the TEARDOWN sink, backoff policy) is specified
in [ADR-0004](../architecture-decisions/0004-crash-only-state-machine.md) and
implemented in `internal/statemachine`. The code is the authority on states
and deadline defaults; this doc deliberately does not re-enumerate them.

Per-state work is delegated through interfaces (`ImageEnsurer`, `Cloner`,
`vm.Manager`, `Dialer`, `GitHub`), which is what lets the FSM's guarantees be
tested with fakes on any OS while the darwin-only implementations stay thin.

## Package map

| Package | Owns |
| --- | --- |
| `internal/home` | the `~/.runny` layout and the config schema (parse/default/validate once, at the boundary) |
| `internal/cycle` | cycle.json records: write/read/prune, retention |
| `internal/tart` | tart bundle format: config.json parse, validation, clonefile |
| `internal/oci` | tart-format image pull: registry auth, manifest, Apple-LZ4 disk assembly, stall detection |
| `internal/sshx` | the only constructor of SSH clients (deadline recipe) |
| `internal/guest` | what to *do* over SSH: stage runner from the virtiofs share, run.sh, diag pull |
| `internal/github` | App JWT → installation token → JIT config; list/delete for reconcile |
| `internal/vm` | Virtualization.framework boot (darwin), dhcpd-lease IP resolution |
| `internal/statemachine` | the FSM; depends only on the seams above |
| `internal/logring` | slog → file sink + in-memory ring for StreamLogs |
| `internal/socket` | the gRPC server over the unix socket |

Dependency direction: `cmd/runnyd` wires everything; `internal/statemachine`
sees only interfaces; leaf packages see nothing of each other (exception:
`guest` and `vm` import `sshx`/`tart` respectively — format and transport are
their job).

## Daemon lifecycle

1. **Load + validate**: config parse (strict), then the doctor suite —
   platform, the 2-macOS-guest cap, `administration:write` asserted on a
   minted token, image resolvability, `df`-based disk headroom. Any failure
   refuses startup loudly.
2. **Sweep** (cold start owns the world): delete `vms/*`, deregister offline
   runners carrying our name prefix.
3. **Prime**: ensure the actions-runner tarball is in the cache share.
4. **Run**: one goroutine per slot + the socket server. SIGINT/SIGTERM cancels
   the root context; in-flight cycles fail into TEARDOWN (which detaches from
   the root context so cleanup always completes), then the daemon exits.

## On-disk layout

`internal/home` is the authority. Shape: `config.yaml`, `runnyd.sock` (0600),
`logs/runnyd.log`, `images/<ref>/<digest>/` (immutable cache),
`vms/<slot>/` (ephemeral, swept), `cache/actions-runner/` (virtiofs share),
`cycles/<slot>/<started>-<id>/cycle.json` (+ post-mortem artifacts on
failure cycles; success cycles keep only the record).
