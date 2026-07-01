# Clipse — session handoff

**Date:** 2026-07-01 · **Phase:** starting execution at Phase 0 (nothing built yet)

Resume brief for a fresh Claude Code session (terminal). Read this, then the design + plan, then execute.

## What this is

Clipse turns Linear issues into merged PRs. A Go **dispatcher/kernel** polls Linear, atomically claims work into local SQLite, spawns per-issue Python **LangGraph + DAC** worker subprocesses in git worktrees, and owns every board transition off the worker's typed JSON result. Full rationale + decision log (A–N) in the design doc.

- Design: [docs/design/2026-07-01-clipse-design.md](design/2026-07-01-clipse-design.md)
- Plan (phased, checkbox work + acceptance criteria): [docs/plans/2026-07-01-clipse-implementation-plan.md](plans/2026-07-01-clipse-implementation-plan.md)
- Brainstorm canvases: Obsidian vault `10_projects/Clipse/` (`Clipse Architecture.canvas` is the clean one).

## How to execute

Use the `superpowers:subagent-driven-development` skill (subagent-driven mode): fresh implementer subagent per task, task review after each, broad review at phase end. Check off the plan's checkboxes as work lands; keep the SDD progress ledger.

TDD (failing test first), structured logging only (`slog` JSON), conventional/lowercase commits, one concern per commit. Ask before adding deps beyond the stack in the plan.

## Scope for this run

**Phase 0 + Phase 1 only** — the self-contained, zero-LLM Go kernel. **Stop after Phase 1's acceptance criteria pass.**

Phase 2–3 are **blocked** pending:
- DAC API spike — verify `deepagents_code` headless run, structured result, and non-interactive thread resume against source/docs (do not guess the API).
- `ANTHROPIC_API_KEY` + `gh` auth.
- A real Linear board (columns + `agent:<lane>` labels) and a target repo.

## Environment (verified 2026-07-01)

- go 1.26.1, uv 0.11.6, python 3.13.7, gh 2.88.1, git 2.50.1 — all present.
- Repo is **not** git-init'd. Phase 0 task 1 = `git init` + a feature branch (do not build on an implied `main`).
- **Sandbox/network:** `go mod download` / `uv sync` need network — allow it or disable the sandbox for those steps.

## Immediate next steps

1. `git init`, create branch (e.g. `feat/phase-0-1-kernel`), first commit.
2. Execute Phase 0 (scaffold + `schema/` contract + codegen) to its acceptance criteria.
3. Execute Phase 1 (Go kernel + TUI vs `testworker` + mock Linear) test-first to its acceptance criteria — including `go test -race`, no-double-claim, caps, crash/stale recovery, failures→Blocked.
4. Stop; report Phase 1 acceptance status.
