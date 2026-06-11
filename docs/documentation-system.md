# Documentation system

This is the rubric every other doc in this repo points at: where each kind of knowledge lives, which genres may go stale, and what stays deliberately un-automated. When you're unsure where something goes, the answer is here.

## 1. One canonical home per concept

Every fact has exactly one home. Non-duplication is the credibility test: the moment a fact lives in two places, a reader can't tell which copy is current, and both lose authority. When two docs would describe the same thing, one links to the other instead of repeating it.

Concretely: the state machine, the on-disk layout, and the system shape live in `docs/architecture/` (with their governing diagrams in the ADRs that own the decisions — ADR-0004, ADR-0006). A package's contracts and sharp edges live in that package's `CLAUDE.md`. If you catch yourself copying a paragraph, stop and link instead.

## 2. Genre boundaries

**`docs/architecture/`** is the current shape: components, how they fit, why the structure is what it is. Maintained; tracks the code. No history, no "we used to".

**`docs/architecture-decisions/`** holds durable decisions, one per file, each with the alternatives that were weighed. Write an ADR when both hold: a **real fork** existed (a different reasonable engineer could have chosen otherwise, with lasting consequences) and the decision **constrains the built artifact** — code, tests, repo layout, or an enforced convention. Mechanical process stays in `CONTRIBUTING.md` even when it had a fork. Amendments are graded: a dated **Amended:** banner for substance changes, a light note for framing shifts, nothing for renames; supersede/superseded back-pointers land on both ADRs in the same commit.

Pre-implementation planning is not a tracked genre. Decisions worth keeping graduate into an ADR (or the architecture docs); the working notes that produced them stay outside the repo. The bootstrap-era design plan was retired (2026-06-09) once its durable content had graduated — git history keeps the text.

**`CONTRIBUTING.md`** is the human dev workflow. **Root `CLAUDE.md`** is the agent index, including the planning protocol — agent-facing process guidance lives there deliberately, not here.

**`docs/deploy.md`** is the operator-facing install/operations guide: host-level procedure (TCC grant, LaunchAgent, migration, rollback). Maintained like architecture — tracks current behavior, carries no phase artifacts (ticket numbers, host names, one-time runbooks).

**Directory `CLAUDE.md`** is a thin pointer to the canonical doc plus reactively-accreted sharp edges — the gotcha that bit someone, never speculation. A directory earns one when it has edges worth recording, not reflexively. Retire a sharp edge in the same commit that deletes its referent.

## 3. Reference content is read from source, never enumerated in prose

Counts and lists drift the instant code changes. The canonical sources here:

- **The FSM** (`internal/statemachine`) — the authority on states, transitions, and deadline defaults. No doc states a state count.
- **The proto file** (`proto/runny/v1`) — the authority on the control surface. No doc lists the RPCs.
- **The config schema** (`internal/home`) — the authority on config keys and defaults. No doc reproduces a key table.
- **`go.mod` / `MODULE.bazel`** — the authority on dependencies.

## 4. Freshness is git's job, and there is deliberately no staleness gate

No doc carries a `Last verified` stamp — git's last-commit date answers "how fresh" more honestly than a hand-maintained line.

**Recorded non-decisions** (settled 2026-06-09; don't relitigate without new evidence):

- **No automated doc-staleness gate** (code-path → doc manifest). It would false-positive on every "former-X" annotation and frozen historical note, training people to ignore it. Mechanical gates are reserved for near-zero-false-positive checks: commitlint, format, build, test. Doc freshness is a judgment call backed by docs-ride-the-commit discipline.
- **No `CLAUDE.md`↔`AGENTS.md` symlink-existence check.** Discipline plus git's native rename-tracking carry it; build the gate only if discipline actually slips.
- **No committed generated code** (and therefore no byte-equality doc gates for it) — the `runny.v1` contract generates in-graph, where drift is structurally impossible (ADR-0006).
