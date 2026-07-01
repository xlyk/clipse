# Clipse — session handoff

**Date:** 2026-07-01 · **Phase:** 0 + 1 complete (on draft PR #1); resuming at **Phase 2** (prerequisite-gated — DAC spike done, board + target repo pending).

Resume brief for a fresh Claude Code session (terminal). Read this, then the guide + design + plan, then execute.

## What this is

Clipse turns Linear issues into merged PRs. A deterministic Go **dispatcher/kernel** polls Linear, atomically claims work into local SQLite, spawns per-issue Python **LangGraph + DAC** worker subprocesses in git worktrees, and owns every board transition off the worker's typed JSON result. The kernel is LLM-free; the LLM lives only in the worker. Full rationale + decision log (A–O) in the design doc.

- Guide (architecture, invariants, conventions, commands): [AGENTS.md](../AGENTS.md)
- Design: [docs/design/2026-07-01-clipse-design.md](design/2026-07-01-clipse-design.md)
- Plan (phased checkbox work + acceptance criteria): [docs/plans/2026-07-01-clipse-implementation-plan.md](plans/2026-07-01-clipse-implementation-plan.md)
- Applied code-review amendments: [docs/plans/2026-07-01-plan-amendments.md](plans/2026-07-01-plan-amendments.md)
- SDD ledger (what shipped, commit SHAs, open follow-ups): `.superpowers/sdd/progress.md`

## Current state

Phase 0 (scaffold + JSON-Schema contract + cross-language codegen + CI drift guard) and Phase 1 (the complete zero-LLM Go kernel) are **done** and on **draft PR #1** (`github.com/xlyk/clipse`, private; branch `feat/phase-0-1-kernel`, merged to `main`). All 11 Phase-1 acceptance criteria pass; `go test -race ./...` clean; CI green (go + python + codegen-drift). Kernel: `internal/{config,store,board,spawn,linear,contract}` + `dispatcher/` + `cli/` (`dispatch`/`status`/`tui`) + `testworker/`. Phase 0+1 plan checkboxes are ticked; Phase 2–4 are unchecked.

Phase-2 work continues on branch `feat/phase-2-coder`.

## Scope for this run: Phase 2 (real DAC coder worker) — GATED

Before writing any Phase-2 code, verify these prerequisites and **STOP + report** if any is missing (do not fake or guess around them):

1. **DAC API spike** (do first, blocking) — ✅ **DONE 2026-07-01** against `deepagents_code` 0.1.22. Findings recorded in the design doc's "DAC API spike findings" subsection. Net: wrap DAC as an **in-process LangGraph graph** via `create_cli_agent(...)` (do not shell out to `dcode -n`); the kernel owns the `AsyncSqliteSaver` checkpointer + `thread_id`; **the worker must use `auto_approve=False, interrupt_shell_only=True`** or the shell allow-list is silently dropped.
2. `ANTHROPIC_API_KEY` — ✅ available (`../agents/apps/estimator-v2/.env`; `sk-ant…`). `gh` — ✅ authenticated (`xlyk`, `repo` scope).
3. Linear board — ⏳ **workspace `clipse-development` being created**; needs columns `Rework`/`Merging`/`Documentation` and labels `agent:coder|reviewer|git_operator|scribe`; candidate-issue query + branch auto-link verified against it. **Access it via the `linear` CLI (`linear --workspace clipse-development …`), not the MCP.**
4. Target repo — ❌ **not yet**: a throwaway repo with required checks + branch protection.

If prerequisites aren't ready, do the DAC spike + any doc updates, then stop.

## How to execute

Use `superpowers:subagent-driven-development`. In a normal interactive session, **`Workflow`-tool orchestration and background agents are fine** — orchestrate with the Workflow tool and sonnet-5 sub-agents. (Only if a workflow ever dies on a process cycle: fall back to **foreground `Agent` tool calls** and resume via `resumeFromRunId`.) Review each committed diff yourself (controller), dispatch a sonnet-5 fixer for Critical/Important findings, and run a broad whole-branch review at the end. Track progress in the plan checkboxes AND `.superpowers/sdd/progress.md`. Work on a feature branch → draft PR; never commit to `main` directly.

## Hard constraints / invariants (do not break)

- **TDD** (failing test first); `make test` is the gate; also `go test -race ./...`.
- The kernel is LLM-free and its tests use zero LLM/network; the worker is the only LLM. Never let the kernel import Python or the worker touch the kernel DB — the seam is the subprocess + typed-JSON contract.
- `schema/*.json` is the source of truth; `make codegen` regenerates both sides (do-not-edit the generated files); CI fails on drift.
- Preserve kernel invariants: `running` only via CAS claim; SQLite is truth, Linear written only via the outbox; failures park in `blocked` (no auto-retry); bare lane in the store; `block_kind` present iff blocked.
- **DAC worker shell safety (spike 2026-07-01):** the worker builds its agent with `auto_approve=False, interrupt_shell_only=True, shell_allow_list=[…]`. `auto_approve=True` silently disables the allow-list (`agent.py:1336/1597/1612`) — never use it as the sole boundary.
- Structured logging only (slog JSON). Conventional/lowercase commits, one concern each, no AI signature. Never `git add -A`. Open PRs as drafts; push with `--force-with-lease`; never `--no-verify`. Ask before adding deps beyond the stack in AGENTS.md. When a Phase-2 finding contradicts the design doc (esp. the DAC API), update the design doc first, then the plan, then code.

## Known follow-ups to fold into Phase 2/3 (see ledger)

- **Cross-lane claiming**: reviewer/git-op/scribe issues sit at `review`/`merging`/`documentation`, but `store.ClaimReady` only claims `ready`→`running` — decide handoff-spawn vs. per-lane-entry claiming (Phase 3).
- Wire `dispatcher.Workspacer.Remove` on the terminal (`done`) cleanup transition.
- Linear `SetState` needs column → per-team-workflow-state-id resolution (do against `clipse-development`).
- Per the Phase-2 plan: env-scrubbing allow-list (worker never sees `LINEAR_API_KEY`), per-issue checkpointer db + cleanup, `max_tokens_per_run` ceiling, idempotent `open_PR`.
- **Pin `deepagents-code==0.1.22`** (`agent/pyproject.toml`) — every DAC symbol we import is internal/pre-1.0 (spike finding #5).

## Environment

- Toolchain (verify): go (per `go.mod`, 1.25 floor), uv, python 3.13+, gh, git — all present as of 2026-07-01.
- `deepagents-code` 0.1.22 + `langgraph` resolve and are installed in `agent/.venv`.
- Sandbox/network: `go mod download` / `uv sync` / `git push` need network — allow it or disable the sandbox for those steps.

## Immediate next steps

1. Read AGENTS.md + design + plan + amendments + ledger.
2. DAC API spike — ✅ done; findings in the design doc.
3. Verify the remaining prerequisites: Linear board (`clipse-development`, via the `linear` CLI) + throwaway target repo with branch protection.
4. Once the board + repo exist, report the Phase-2 implementation plan (prerequisite status first) before writing worker code.
