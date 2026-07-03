# Clipse — session handoff

**Date:** 2026-07-02 · **Branch:** `feat/phase-3-pipeline` · **Gate:** `make test` + `go test -race ./...` + `make lint` green.

Resume brief for a fresh session. Read this, then the guide + design + plan, then continue.

## Current state

The full pipeline is implemented. Phase 0/1 (the zero-LLM Go kernel), Phase 2
(the DAC Coder worker), and Phase 3 (the Reviewer and Git-operator lanes,
cross-lane per-column claiming, and the rework cap) all run. A live smoke took a
10-issue dependency DAG on a real Linear board through to merged PRs on `main`.

**Documentation is now a step inside the coder graph, not a separate lane.**
The `run_docs` node (`graphs/coder.py`) writes docs into the coder's own
worktree right before the PR is opened, so they ride the same commit/PR and get
reviewed. A merged PR goes `merging → done` directly — the `documentation`
column, the `scribe` lane, and the `-docs` branch/worktree apparatus are gone.
(This retires the "Scribe writes docs on its own branch" note below.)

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

- **Scribe writes docs on its own branch.** _(Superseded: documentation is now
  a coder-graph step — see Current state. The `-docs` branch existed only
  because the scribe ran post-merge.)_ Docs landed on a `-docs` branch off
  `origin/<base>`, so the Scribe never force-pushed the merged branch (`964b3c3`).
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
- **Reviewer feedback reaches the Coder on rework.** The dispatcher threads the
  latest `changes_requested` summary into the coder's rework re-run (env
  `CLIPSE_REVIEW_FEEDBACK`), so a changes-requested issue converges instead of
  re-emitting the identical diff and tripping the rework cap (`e8705c8`).
- **Auto-unblock layer 1.** Transient / crash / timeout / spawn failures
  auto-retry to their release column, bounded by `recover_cap` (default 5) + a
  `blocked_until` backoff, then park. Non-transient blocks
  (`capability`/`needs_input`), rework-cap, illegal transitions, and orphan
  `max_attempts` still park (`a61ecca`, `9f97944`).
- **Fullscreen TUI redesign.** Alt-screen (no scrollback bleed, restores on
  quit), dense header, dashboard/kanban tabs, PIPELINE above a full-width
  ACTIVITY feed; rendered + iterated via VHS screenshots (`7e89988`).

## Live smoke

A 10-issue "greet" dependency DAG on the `clipse-development` Linear board (team
`CLI`) against `xlyk/clipse`, run end to end: **all 10 merged to `main` in
dependency order**, all four lanes exercised. CLI-15 (which had hit the rework
cap under the old byte-identical dead-loop) converged once the rework-feedback
fix was live — the exact bug it was built for. The smoke config lives at
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

## Demo tooling

`scripts/demo/demo.sh` preps a clean-slate run and shows it live: reset → build
→ seed the DAG → (waits for ENTER) → dispatcher in the background + the live TUI
in the foreground. You arrange the windows and record the screen yourself; the
script does only the prep + launch. `--full` runs the 10-ticket DAG, `--keep`
retains the board. (The old `record-demo.sh` + `arrange-windows.applescript`,
which drove ffmpeg + window placement, were removed — too brittle across macOS
TCC/Accessibility grants.)

## Verified: board transitions DO mirror to Linear — Linear coalesces the display

_(Investigated when `documentation` was still a column; that column has since
been removed — see Current state — but the coalescing insight still applies to
the remaining fast hops like `merging`.)_ A "cards skip stages on the board"
report was investigated and the kernel is **correct**: the `linear_writes`
table showed every hop per issue sent with `status=done`, zero retries, zero
errors. The board's *current* state is always right.

The blips are **Linear's own activity/history coalescing**: rapid successive
state changes by the same actor collapse in Linear's history — the faster a card
transits a column, the more Linear collapses it. Decision: **leave the kernel
as-is** (no artificial per-column dwell in a deterministic production kernel).
The **TUI is the faithful surface** — it reads SQLite, which never coalesces,
and shows every working lane live.

## Open follow-ups

- **TUI live-agent visibility — shipped the cheap half; worker internals are the follow-up.**
  Liveness is now per-row, keyed off the held claim, so the spinner + the
  working-lane badge + elapsed light up for the reviewer/git_operator
  agents too (not just the coder "running" row), and the header shows a
  `⚡ N working` tally. What's still missing is *what* each worker is doing
  mid-run: the captured worker log (`<board>/logs/<issue>.log`) is only DAC
  skill-load noise, not a step trace. To show live tool-calls/steps, instrument
  the DAC/LangGraph worker to emit structured progress on a clean channel, have
  the dispatcher record it, and tail it per-agent in the TUI.
- **Thread the reviewer's inline findings, not just its rollup summary.**
  Rework feedback (`CLIPSE_REVIEW_FEEDBACK`) currently carries the reviewer
  run's terse summary, which can go vague ("diff unchanged, same findings")
  rather than the actionable per-line findings. CLI-15 still converged (the
  coder recovered specifics from the PR/context), but threading the reviewer's
  inline comments (or making the reviewer always restate findings in its
  summary) would make convergence robust rather than model-luck.
- **GitHub App bot identity.** Give Clipse its own `clipse[bot]` identity for
  attribution and security, replacing the owner's PAT. Design + how-to in
  [docs/design/2026-07-02-github-app-bot-identity.md](design/2026-07-02-github-app-bot-identity.md).
- **HITL answer → resume channel.** A `blocked(needs_input)` issue that is
  requeued re-runs as a fresh turn, not a resume with the human's answer fed
  back into the pending interrupt. Design the answer → `Command(resume=…)`
  channel (e.g. sourced from a Linear comment).
- **Smoke cleanup.** `samples/greet/*` from the smoke was merged into
  `xlyk/clipse` `main`; delete it once you're done demoing.
