# Coder base-sync + merge-conflict resolution — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]`.

**Goal:** every coder turn syncs its worktree branch to current `origin/<base>` before running (fixes dependents building on a stale base), and a real merge conflict is *resolved by the coder* (merge markers surfaced in the worktree, DAC resolves them, platform commits) instead of looping to `blocked`.

**Why (design B, chosen):** clipse already detects a PR conflict (`internal/gitops.hasConflict` → `gh pr update-branch` → `OutcomeStaleBaseConflict` → dispatcher routes `merging→rework` naming the files). But the rework is a no-op today: `make_ensure_worktree` only *validates* the worktree — it never merges `origin/<base>`, so the re-run coder sees no conflict markers, re-runs its task on the stale base, re-conflicts, loops → `rework_cap` → blocked. Syncing the base every turn surfaces the conflict as markers the coder can edit, and also fixes the base-freshness bug (a dependent cut before deps merged branches off stale local main).

**Tech Stack:** Go 1.25 (spawn/config), Python 3.13 + uv (LangGraph coder graph).

## Global Constraints
- **Merge, not rebase:** `git merge --no-edit origin/<base>` → a merge commit; `git push` stays fast-forward (NO force-push). Never rewrite pushed history.
- The **graph node** runs all git (`fetch`/`merge`/`commit`); the **DAC agent only edits files** to resolve conflict markers — so the coder's "don't run git to commit/push/PR" prompt contract is unchanged. No new SAFETY surface.
- Base-sync is best-effort for freshness: a non-conflict failure (offline `fetch`, unexpected merge error) → `git merge --abort` if needed, log a warning, proceed on the un-synced base (do NOT fail the turn on a transient fetch). A genuine conflict is the expected non-clean path → resolution.
- Go: wrap errors (`%w`); table-driven stdlib tests. Python: injectable `run_command` (no real git in unit tests, matching the existing coder-graph test style); pydantic v2; ruff. Commits: Conventional, lowercase, no trailing period, no AI signature. `make test` gate.

## File Structure / order
1. `internal/spawn/{spawn.go,local.go}` + `dispatcher/spawn.go` — thread `base_branch` to the worker (`WorkerSpec.BaseBranch`, `--base-branch`, set from `cfg.Repo.BaseBranch`).
2. `agent/src/clipse_agent/worker.py` — `--base-branch` arg → `state["base_branch"]`.
3. `agent/src/clipse_agent/graphs/coder.py` — new `sync_base` node (`ensure_worktree → sync_base → run_DAC`); conflict → `state["merge_conflict_files"]`; conflict-resolution task text; `commit` completes the merge.
4. `AGENTS.md` — document base-sync-every-turn + conflict resolution + `base_branch` threading.

---

### Task 1: Go — thread `base_branch` to the worker

**Files:** `internal/spawn/spawn.go` (WorkerSpec), `internal/spawn/local.go` (workerArgs), `dispatcher/spawn.go` (spawnAttempt). Tests: `internal/spawn/argv_test.go`, `dispatcher/*_test.go`.

**Interfaces:** `WorkerSpec.BaseBranch string`; `workerArgs` emits `--base-branch=<v>` when non-empty; `spawnAttempt` sets it from `d.cfg.Repo.BaseBranch`.

- [ ] **Step 1: Failing tests.** `TestWorkerArgs`: `BaseBranch: "main"` → argv contains `--base-branch=main`; empty → flag omitted. A dispatcher test (mirror the model-wiring integration test) asserting `fakeSpawner.Specs()[0].BaseBranch == cfg.Repo.BaseBranch` on a coder claim.
- [ ] **Step 2:** run → fail.
- [ ] **Step 3:** add `BaseBranch string` to `WorkerSpec` (comment: the repo base the coder syncs its worktree to each turn). In `workerArgs`, after `--docs-model-params`, append `--base-branch=`+v when non-empty. In `spawnAttempt`, set `BaseBranch: d.cfg.Repo.BaseBranch` in the `WorkerSpec` literal (all lanes; harmless for reviewer/gitops which don't sync).
- [ ] **Step 4:** `go test ./internal/spawn/ ./dispatcher/ -v && go test -race ./...` → pass.
- [ ] **Step 5:** commit `feat(spawn): thread repo base_branch to the worker`.

---

### Task 2: Python — `--base-branch` arg + `sync_base` node

**Files:** `agent/src/clipse_agent/worker.py`, `agent/src/clipse_agent/graphs/coder.py`. Test: `agent/tests/test_worker.py`, `test_coder_graph.py`.

**Interfaces:** worker `--base-branch` (default `""`) → `state["base_branch"]`; new node `make_sync_base(run_command)` inserted `ensure_worktree → sync_base → run_DAC`; produces `state["merge_conflict_files"]: list[str]` (empty when clean/skipped).

- [ ] **Step 1: Failing tests** (`test_coder_graph.py`, injectable `run_command` fake — follow the existing node-test pattern): (a) clean merge → `merge_conflict_files == []`, proceeds to run_DAC; (b) merge exits non-zero with unmerged files → `git diff --name-only --diff-filter=U` output parsed into `merge_conflict_files`, node does NOT raise; (c) `base_branch` empty → node is a no-op (skips sync); (d) `fetch` fails → warning, `git merge --abort` not needed, proceeds (no raise, `merge_conflict_files == []`). `test_worker.py`: `--base-branch=main` threads into `state["base_branch"]`.
- [ ] **Step 2:** run → fail.
- [ ] **Step 3:** worker.py: add `--base-branch` arg; `_run_lane_graph`/`_dispatch` put it into the graph input state (`"base_branch"`). coder.py: `make_sync_base(run_command)` node — if `state["base_branch"]` empty, return `{"merge_conflict_files": []}`; else `git fetch origin <base>` (on failure: log, return `{"merge_conflict_files": []}`), then `git merge --no-edit origin/<base>` in `state["cwd"]`; on success `{"merge_conflict_files": []}`; on conflict, parse `git diff --name-only --diff-filter=U` → `{"merge_conflict_files": [...]}` (leave the merge in progress for the coder to resolve). Wire `add_node("sync_base", ...)`, replace edge `ensure_worktree→run_DAC` with `ensure_worktree→sync_base→run_DAC`. Add `merge_conflict_files` to `CoderState`.
- [ ] **Step 4:** `cd agent && uv run pytest -q` → pass.
- [ ] **Step 5:** commit `feat(coder): sync worktree to origin base each turn`.

---

### Task 3: Python — conflict-resolution task + commit the merge

**Files:** `agent/src/clipse_agent/graphs/coder.py` (`load_context`/`run_DAC` task text; `make_commit`). Test: `test_coder_graph.py`.

- [ ] **Step 1: Failing tests:** (a) when `merge_conflict_files` non-empty, the DAC task text instructs resolving the conflict markers (`<<<<<<<`/`=======`/`>>>>>>>`) in exactly those files, preserving both intents, then stopping — and does NOT include the normal issue task (or clearly frames the turn as conflict-resolution); when empty, the normal issue task is used. (b) `make_commit`: with a merge in progress + resolved files, it stages (`git add -A` within the worktree is fine here — it's the coder's own worktree, NOT the clipse repo) and completes the merge commit (`git commit --no-edit`), then the existing push runs (fast-forward). Confirm no `git add` touches anything outside the worktree.
- [ ] **Step 2:** run → fail.
- [ ] **Step 3:** in the DAC-task construction, branch on `state.get("merge_conflict_files")`: if non-empty, build a conflict-resolution instruction naming the files; else the existing issue/rework task. `make_commit`: detect an in-progress merge (e.g. `.git/MERGE_HEAD` or `git rev-parse -q --verify MERGE_HEAD`) or simply that `merge_conflict_files` was set, then stage + `git commit --no-edit` to complete the merge before/instead of the normal commit path (keep the normal commit when no merge in progress).
- [ ] **Step 4:** `cd agent && uv run pytest -q` → pass.
- [ ] **Step 5:** commit `feat(coder): resolve merge conflicts in the coder turn`.

---

### Task 4: docs + gate
- [ ] **Step 1:** AGENTS.md: the coder syncs its worktree to `origin/<base_branch>` at the start of every turn (fixes stale-base dependents); a real merge conflict surfaces as markers the coder resolves (graph does the git; DAC edits files; platform commits the merge) rather than looping to blocked; `base_branch` is threaded to the worker via `--base-branch`.
- [ ] **Step 2:** `make codegen && git diff --exit-code` (no schema) ; `make test && make lint`.
- [ ] **Step 3:** commit `docs: coder base-sync + merge-conflict resolution`.

## Self-review
- Confirm the merge is `--no-edit` + push stays FF (no force). Confirm the DAC agent never runs git for the merge/commit (graph does) — SAFETY prompt unchanged.
- Confirm `git add` in `make_commit` is scoped to the coder's worktree cwd, never the clipse repo.
- Confirm base-sync is best-effort (fetch failure → proceed, don't block the turn).
- Open risk: reviewer/git_operator lanes also receive `--base-branch` but never run `sync_base` (only the coder graph has it) — harmless; note it.
