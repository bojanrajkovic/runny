# Design plan: skeleton and daemon MVP

**Goal:** a fully featured `runnyd` + `runnyctl` pair — boot real runner VMs
from tart-format bundles, register via JIT config, run jobs, recycle, all
observable over the socket. RunnyBar follows in a later plan.

Grounding: the FSM, on-disk layout, and deadline table live in the project's
ideation doc and graduate into `docs/architecture/` as they are implemented;
the decisions are ADR-0001…0008.

## Milestones (each lands as one or more atomic commits on main)

1. **Skeleton** — `proto/runny/v1` with the control service; `cmd/runnyd` and
   `cmd/runnyctl` hello-world mains; gazelle-managed BUILD files;
   `bazel build //...` + `bazel test //...` green on Linux.
2. **`internal/home`** — the `~/.runny` layout: path resolution, config
   (YAML), cycle artifact writing, retention pruning. Pure Go, tested.
3. **`internal/tart`** — bundle `config.json` parse (base64
   hardwareModel/ecid), bundle clone via `unix.Clonefile` (darwin),
   `diskFormat: raw` enforcement. Format layer is OS-independent and tested;
   clonefile is darwin-tagged.
4. **`internal/sshx`** — the deadline recipe (ADR-0002): bounded dial,
   streamed exec, exit codes, scp-style file pull for post-mortems. Tested
   against a local ssh server stub (TCP-accept-no-banner, black-hole).
5. **`internal/github`** — App JWT, installation token, generate-jitconfig,
   runner list/delete, with retries+jitter. Tested against httptest stubs.
6. **`internal/oci`** — tart-format image pull: token auth (ghcr), manifest,
   concurrent LZ4 layer assembly to `disk.img`, digest verification.
   Tested against httptest registry stubs.
7. **`internal/vm`** (darwin) — vz boot from a bundle: VZ config from parsed
   tart config, fresh ECID/MAC, dhcpd-lease IP resolution, virtiofs runner
   cache share, stop/force-stop. Interface seam (`vm.Manager`) so the FSM
   tests on Linux against a fake.
8. **`internal/statemachine`** — the 11-state FSM over the seams from 2–7:
   per-state `context.WithDeadline`, event channel, backoff, cycle.json,
   post-mortem collection, GitHub reconcile. The core deliverable; heavily
   tested with fakes (deadline expiry, zombie detection, teardown escalation).
9. **`internal/socket` + daemon assembly** — gRPC over the unix socket
   (Status/Watch, Logs, Recycle, Pause/Resume, Why, Doctor); `cmd/runnyd`
   startup validation + sweep + slot supervisor; `log/slog` file sink + ring
   buffer.
10. **`runnyctl`** — subcommands over the socket client; human-readable
    rendering of status/why; `--json` for scripting.
11. **End-to-end on ix** — sign with the virtualization entitlement, run a
    full cycle against the cached Tahoe image, verify `runnyctl status/why`,
    document in `docs/architecture/`.

## Out of scope (this plan)

RunnyBar implementation; release pipeline (Phase 4); launchd deployment
(Phase 5); ASIF disk support; Orchard-style multi-host.

## Test posture

Pure-Go packages: table tests + httptest stubs, runnable on Linux and in the
Linux CI job. Darwin-only code is thin and interface-fronted; its coverage
comes from the ix end-to-end pass (milestone 11) until a macOS CI job lands in
Phase 3.
