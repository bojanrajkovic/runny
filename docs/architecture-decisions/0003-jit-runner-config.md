# ADR-0003: JIT runner config over registration tokens

**Status:** Accepted (2026-06-07)

## Context

GitHub offers two ways to register a self-hosted runner: a registration token
handed to `config.sh` inside the guest, or `generate-jitconfig`, which mints an
encoded one-shot config the runner consumes via `./run.sh --jitconfig`.

## Decision

runnyd mints JIT configs via a GitHub App (App JWT → installation token →
`POST .../actions/runners/generate-jitconfig`) and passes the encoded blob to
the guest. No token handoff into the guest, no `config.sh`, auto-removal after
one job — purpose-built for ephemeral runners.

Spike-proven end-to-end 2026-06-07 (container start → `Listening for Jobs` in
~24 s).

## Operational constraints (spike-learned, enforced by `runnyd doctor`)

- The App's installation token must actually carry `administration: write` —
  assert it on a minted token, not just in App settings (permission upgrades
  generally queue per-installation approval).
- `runner_group_id: 1` is the default group for repo-level runners.
- A JIT runner that never takes a job lingers as `offline` until explicitly
  `DELETE`d — teardown must delete on the no-job path.

## Rejected alternatives

- **Registration tokens + config.sh**: token material enters the guest,
  requires explicit deregistration on every path, and `config.sh` state
  persists across runs — all liabilities for crash-only ephemeral VMs.
