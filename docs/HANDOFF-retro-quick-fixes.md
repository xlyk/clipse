# Handoff — execute the retro quick-fixes plan

- **Date:** 2026-07-04
- **Next-session goal:** implement `docs/plans/2026-07-04-retro-quick-fixes-implementation.md` (this branch) — 20 TDD tasks fixing the failure classes the Reflex MVP build exposed, plus the per-run handoff-comments loop.
- **Method:** superpowers subagent-driven development with opus sub-agents. Each task in the plan is a complete work item with failing-test code, implementation code, run commands, and acceptance criteria — delegate whole tasks, run independent ones concurrently.

## Where everything is

- **Plan:** `docs/plans/2026-07-04-retro-quick-fixes-implementation.md` on branch `plan/retro-quick-fixes` (draft PR xlyk/clipse#23). Worktree already exists at `/Users/xlyk/Code/clipse/.claude/worktrees/retro-fixes-plan`.
- **Evidence:** the retro that produced the plan is `xlyk/reflex-v2` `docs/clipse-retro.md` (draft PR xlyk/reflex-v2#36). Read it if a task's "why" needs more depth — every task cites its incident.
- **Existing fix to land, not rewrite:** branch `fix/reviewer-skip-binary-images` (open draft PR xlyk/clipse#22) is Task 16. Rebase, test, merge — do not reimplement.

## How to run it

1. Start from the plan worktree (or `git switch plan/retro-quick-fixes` in a fresh worktree). Implementation goes on **new feature branches off `origin/main`**, one per part — the plan branch is docs-only.
2. Batches and ordering (from the plan's final section): A (tasks 1–4, 6–9, Go gitops/dispatcher/spawn) → B (task 5, depends on 6's `Retriable` field) → C (10–13, Python coder) → D (14–16, reviewer) → E (17–20, handoff loop; 17 before 18/19, 20 last). A, C, D are mutually independent — safe to run concurrently on separate branches/worktrees. Chain inside C: 10 → 11 → 12.
3. One draft PR per part (A–E), lowercase terse conventional commits, one task per commit as the plan's step 5s specify. `git push --force-with-lease` only; never `--no-verify`.
4. Verify per task with the plan's exact commands; per part: `go test ./...` from the repo root, `cd agent && uv run pytest -q && uv run ruff check`. `make build` before calling a Go part done.

## Gotchas (from reading the source while writing the plan)

- **The worker contract is triple-sourced:** `schema/worker-result.schema.json`, `internal/contract/contract.go`, `agent/src/clipse_agent/contract.py` must change together (Task 17). Grep for how `PrUrl` does optional fields before adding `Handoff`.
- **Reuse the gitops test harness.** `internal/gitops/fakegh_test.go` + `testhelpers_test.go` already script `gh` responses; the plan's `fakeGH(t, withPRView(...))` calls are placeholders for whatever those helpers are actually named — adapt, don't build a parallel fake.
- **Tasks 5 and 6 share `gitops.Result.Retriable`** — implement 6 first (or both on one branch) as the plan notes.
- **Task 13 is investigation-first:** confirm where the worker subprocess's cwd is set (`grep -rn "exec.Command" internal/spawn/ dispatcher/`) before fixing; the plan names the suspected mechanism and the fallback fix site.
- **Task 2 touches config defaults:** follow the existing default-setting pattern in `internal/config/config.go` (where `RecoverCap` gets its default) for `repo.require_checks: true`.
- **`reflex.clipse.yaml` at the clipse repo root is untracked and machine-local** — never commit it.
- Existing tests may assert the old behaviors the plan changes (e.g. checksAbsent → NotMergeable). Fixing those assertions is in scope; note it in the task's commit body.

## Out of scope — do not scope-creep

`waiting_ci` claim state, kernel-run verification loop, operator override CLI
verbs, canary board, separate reviewer GitHub identity, drain/hot-reload,
cost rollups. Each needs its own design/plan pass (listed in the retro's
"beyond this build's incidents" section).

## Definition of done

All 20 plan checkboxes checked, five draft PRs open (or fewer if parts were
combined deliberately), full Go + Python suites green on each, and PR #22
merged. Post a summary comment on PR #23 linking the implementation PRs.
