# Clipse — session handoff

**Date:** 2026-07-02 · **Branch:** `feat/phase-3-pipeline` · **Gate:** `make test` + `go test -race ./...` + `make lint` green.

Resume brief for a fresh session. Read this, then the guide + design + plan, then continue.

## Current state

The full pipeline is implemented. Phase 0/1 (the zero-LLM Go kernel), Phase 2
(the DAC Coder worker), and Phase 3 (the Reviewer, Scribe, and Git-operator
lanes, cross-lane per-column claiming, and the rework cap) all run. A live smoke
took a 10-issue dependency DAG on a real Linear board through to merged PRs on
`main`.

The kernel stays LLM-free; the LLM lives only in the worker; the seam is the
subprocess + typed-JSON contract. Kernel invariants hold (see AGENTS.md):
`running` only via CAS claim, SQLite is truth, Linear written only through the
outbox, failures park in `blocked`, bare lane in the store, `block_kind` present
iff blocked.

Reference docs:

- Guide (architecture, invariants, conventions, commands): [AGENTS.md](../AGENTS.md)
- Design: [docs/design/2026-07-01-clipse-design.md](design/2026-07-01-clipse-design.md)
- GitHub App bot identity (follow-up design): [docs/design/2026-07-02-github-app-bot-identity.md](design/2026-07-02-github-app-bot-identity.md)
- Plan (phased checkboxes + acceptance): [docs/plans/2026-07-01-clipse-implementation-plan.md](plans/2026-07-01-clipse-implementation-plan.md)
- Applied code-review amendments: [docs/plans/2026-07-01-plan-amendments.md](plans/2026-07-01-plan-amendments.md)
- SDD ledger (commit SHAs, open follow-ups): `.superpowers/sdd/progress.md`

## What shipped this session

Phase 3 lanes (`20c9ac1`) plus a run of fixes, each its own commit:

- **Scribe writes docs on its own branch.** Docs land on a `-docs` branch off
  `origin/<base>`, so the Scribe never force-pushes or non-ff-pushes the merged
  branch (`964b3c3`).
- **Dependency-direction bug fixed.** The kernel now reads Linear
  `inverseRelations` to find an issue's blockers. Gating was inverted before, so
  promotion order was wrong; it now promotes in true dependency order
  (`29355c3`).
- **Linear comments render as markdown** — emoji headings and fenced code, not
  flat text (`a8b924d`).
- **Reviewer posts `gh pr comment`, not a self-`gh pr review`.** GitHub blocks a
  formal review on your own PR; the verdict flows through the typed result
  instead (`ebcf5c4`, `6f5b1ad`). See the GitHub App design doc for why one
  identity is fine here.
- **Coder leaves git/gh to the platform.** It no longer loops on `gh pr create`;
  the kernel owns commit/push/PR (`1ca66ca`).
- **Git-operator retries a not-yet-ready merge** instead of blocking on it, and
  marks a draft PR ready before merging (`2fc26f9`, `28ae4f3`).
- **`max_tokens_per_run` default raised to 1M** (the smoke config uses 2M)
  (`03f79c2`).
- **Gorgeous bubbletea TUI** — liveness dot, activity feed, deps/progress,
  `j`/`k` + `enter` inspector, and a `tab` kanban view; token counts sum across
  all runs, not just the latest (`c36889f`, `f8314ec`, `855510a`, `6a16332`).

## Live smoke

A 10-issue "greet" dependency DAG on the `clipse-development` Linear board (team
`CLI`) against `xlyk/clipse`. Dependency-ordered promotion is proven end to end;
about 8 of 10 issues merged to `main`. The smoke config lives at
`~/Code/clipse-smoke/clipse.yaml` (local, uncommitted) with a reusable
`seed.sh`.

## How to run

- `make test` — the gate (Go suite + `agent/` pytest). Add `go test -race ./...`.
- `make build` — compile `./bin/clipse`.
- `./bin/clipse dispatch --config <clipse.yaml>` — the daemon (`--board <dir>`
  optional; defaults to config `board_dir`).
- `./bin/clipse tui --board <dir>` — live dashboard. Keys: `?` help, `tab`
  kanban, `enter` detail, `q` quit. (`--board` defaults to `./.clipse`.)
- `./bin/clipse status` — one-shot SQLite snapshot table.

## Open follow-ups

- **Rework feedback → Coder retry prompt** (in progress). Without it, a
  `changes_requested` issue re-runs as a fresh task and can't converge on the
  reviewer's feedback. This is the next thing to finish.
- **Auto-unblock, layer 1.** A bounded, deterministic retry of transient /
  capability / crash blocks. `needs_input` and `rework_cap` exhaustion are
  **not** auto-retryable — they need a human or new input.
- **GitHub App bot identity.** Give Clipse its own `clipse[bot]` identity for
  attribution and security, replacing the owner's PAT. Design + how-to in
  [docs/design/2026-07-02-github-app-bot-identity.md](design/2026-07-02-github-app-bot-identity.md).
- **HITL answer → resume channel.** A `blocked(needs_input)` issue that is
  requeued re-runs as a fresh turn, not a resume with the human's answer fed
  back into the pending interrupt. Design the answer → `Command(resume=…)`
  channel (e.g. sourced from a Linear comment).
- **Smoke cleanup.** `samples/greet/*` from the smoke was merged into
  `xlyk/clipse` `main`; delete it once you're done demoing.
