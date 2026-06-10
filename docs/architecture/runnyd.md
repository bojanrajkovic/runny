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

## Operational sharp edges (e2e-learned, 2026-06-09)

- **macOS Local Network privacy (TCC)**: a background-reparented ad-hoc
  runnyd gets silently denied vmnet access — every guest dial fails with
  `connect: no route to host` while the host shell reaches the same port.
  Foreground children of sshd inherit its exemption. The eventual launchd
  deployment must own the Local Network grant story; until then, run the
  daemon under a held session.
- **Never trust the image's bundled runner**: cirruslabs images preinstall
  `~/actions-runner`, which rots into broker-rejected versions ("deprecated
  and cannot receive messages") that JIT runners cannot self-update out of.
  The provision script always stages from the cache share, which is itself
  sourced from the repo's `/actions/runners/downloads` endpoint — the
  service's own answer to "which build works".
- **Seed the image cache from tart's** when migrating a host: the bundles are
  clonefile-compatible (`cp -c` the four files into
  `images/<ref>/<digest>/`), avoiding an 80GB+ re-pull.

## E2E validation (2026-06-09, ix)

Full chain proven against real infrastructure: cache-hit ENSURE_IMAGE →
clonefile CLONE → vz BOOT (~3s) → dhcpd AWAIT_IP (~6s) → AWAIT_SSH →
MINT_JIT (real App → installation token → jitconfig) → PROVISION (tarball
from virtiofs share) → LISTENING held with the runner `online` on GitHub
with its labels. Operator recycle deregistered the unused runner; SIGTERM
mid-cycle left zero VMs, zero vm dirs, zero registrations. Four defects
found and fixed along the way (Apple-LZ4 dict chaining, pull-progress
invisibility, the TCC denial, runner-version rot) — each surfaced by a
different observability layer doing its job.
