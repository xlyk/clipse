# Clipse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Work items use checkbox (`- [ ]`) syntax for tracking. Within each item, follow TDD: failing test first, then implement.

**Goal:** Build Clipse — an autonomous coding-agent orchestrator that turns Linear issues into merged PRs by dispatching per-issue LangGraph/DAC workers in isolated git worktrees.

**Architecture:** Two planes. A Go **dispatcher/kernel** (single daemon) polls Linear, atomically claims work into a local SQLite runtime store, spawns per-issue worker subprocesses, monitors them, and owns every board transition off the worker's typed JSON result. A Python **worker** (LangGraph graph wrapping DAC) runs the per-issue pipeline and emits that typed result. See [design doc](../design/2026-07-01-clipse-design.md).

**Tech Stack:** Go 1.23+ (cobra CLI, bubbletea+lipgloss TUI, `log/slog` JSON logging, `modernc.org/sqlite` pure-Go driver, WAL); Python 3.13 + uv (LangGraph, `deepagents_code`, Pydantic v2, LangGraph `AsyncSqliteSaver` checkpointer); contract via JSON Schema → `go-jsonschema` + `datamodel-code-generator`; `gh` CLI for PR/merge; secrets via `op run`/env.

## Global Constraints

- **TDD**: failing test first, then minimal implementation. `make test` is the gate.
- **Structured logging only** — `log/slog` JSON handler (Go); OTel-friendly. No `fmt.Println` for runtime logs.
- **SQLite is runtime truth; Linear is task intent.** The dispatcher is the *only* writer of Linear board state.
- **`running` is entered only via CAS claim** (`status='ready' AND claim_lock IS NULL`). Never a direct write.
- **Single dispatcher** — machine-global singleton lock; one daemon per machine.
- **Failures park in `Blocked`** — no failure auto-retry. Continuation is bounded by a per-issue turn cap.
- **Phase-1 tests use zero LLM and zero real network** — Linear is mocked; the worker is `testworker` (canned JSON).
- **Go module**: single root `github.com/xlyk/clipse`; one binary `clipse` (cobra subcommands). Heavy logic in `internal/`.
- **Commits**: Conventional Commits, casual/lowercase. One concern per commit.
- **Ask before adding a dependency** beyond the stack above.

---

## Phase 0 — Repo scaffolding & contract

**Purpose:** Bootstrap the monorepo, the shared contract, and cross-language codegen so both planes compile against the same types. No behavior yet.

**Files / components:** `go.mod`, `cmd/clipse/main.go`, `schema/*.json`, `agent/pyproject.toml`, `Makefile`, `.github/workflows/ci.yml`, `configs/clipse.example.yaml`, dir skeleton per design doc.

### Work
- [ ] Init single Go module `github.com/xlyk/clipse` (Go 1.23+); create `cmd/clipse/main.go` with a cobra root command printing version.
- [ ] Create the folder skeleton: `dispatcher/`, `cli/` (+`cli/tui/`), `internal/{linear,store,board,spawn,contract,config}/`, `agent/`, `testworker/`, `schema/`, `configs/`.
- [ ] Author `schema/worker-result.schema.json` — fields: `run_id`, `issue_id`, `lane`, `outcome` (enum `done|needs_review|changes_requested|blocked|continue`), `block_kind` (enum `needs_input|capability|transient|null`), `summary`, `artifacts[]`, `pr_url?`, `thread_id`, `turn_count`, `tokens{in,out}`. Include `$id`, `required`, `additionalProperties:false`.
- [ ] Author `schema/board.schema.json` — enums for lanes (`coder|reviewer|git_operator|scribe`), columns (`todo|ready|running|review|merging|documentation|done|rework|blocked`), block kinds.
- [ ] Wire `make codegen`: `go-jsonschema` → `internal/contract/*.go`; `datamodel-code-generator` → `agent/src/clipse_agent/contract.py`. Generated files marked do-not-edit.
- [ ] Add `agent/pyproject.toml` (uv), Python 3.13, deps `langgraph`, `deepagents_code`, `pydantic>=2`; `clipse-worker` console entrypoint stub that echoes a valid empty result.
- [ ] Add `Makefile` targets: `build`, `test` (Go + Python), `codegen`, `lint` (`go vet`/`ruff`), `run`.
- [ ] Add `configs/clipse.example.yaml`: `repo{remote,path,base_branch}`, `poll_interval_s`, `caps{global,per_lane{...}}`, `turn_cap`, `max_runtime_s`, `lane_label_prefix: "agent:"`.
- [ ] Add `.github/workflows/ci.yml`: matrix build+test Go and Python; run `make codegen` and fail on diff (drift guard).
- [ ] Add `README.md` (one-paragraph overview + link to design doc) and root `.gitignore`.

### Acceptance criteria
- [ ] `git init` done; `make build` produces a `clipse` binary; `clipse --version` prints.
- [ ] `make codegen` generates Go + Pydantic types from `schema/`; re-running is a no-op (idempotent).
- [ ] CI fails if generated code is stale vs `schema/` (drift guard proven by a deliberate dirty commit in a scratch branch).
- [ ] `make test` runs (green, even if only trivial tests) across both languages.
- [ ] `uv run clipse-worker` emits a schema-valid JSON result on stdout.

---

## Phase 1 — Go kernel + TUI (fake worker, test-first)

**Purpose:** The complete deterministic scheduler, provably correct against a canned-JSON `testworker` and a mocked Linear. Zero LLM. This is the riskiest core; it ships fully tested before any real agent exists.

**Files / components:** `internal/config`, `internal/store` (SQLite), `internal/linear` (client + mock), `internal/board` (state machine), `internal/spawn` (Spawner + local impl), `testworker/main.go`, `dispatcher/` (loop), `cli/` (`status`, `tui`), `cmd/clipse`.

### Work
- [ ] **config** (`internal/config`): load + validate `clipse.yaml` into a typed struct; defaults; fail-fast on bad values. Test: valid/invalid/defaulted cases.
- [ ] **store schema** (`internal/store`): migrations creating `issues`, `runs`, `events` tables (fields per design doc); open DB in WAL mode. Test: migrate-on-empty, idempotent re-migrate.
- [ ] **store CRUD**: upsert issue (from normalized Linear), append event, insert/close run, read snapshot. Test each.
- [ ] **CAS claim** (`store.ClaimReady`): compare-and-swap `status='ready' AND claim_lock IS NULL` → `running` + write `runs` row + `claimed` event. Test: **concurrent-claim test** (N goroutines, exactly one wins).
- [ ] **heartbeat & TTL** (`store`): `Heartbeat(runID)` extends `claim_expires`; `ReleaseStaleClaims(now)` requeues runs past TTL. Test: stale vs fresh.
- [ ] **Linear client** (`internal/linear`): GraphQL candidate-issue query (active states), normalize → internal `Issue` (id, identifier, status, lane label, deps/blockers, priority, branch_name, updated_at). Behind a `Client` interface. Test: normalize from recorded JSON fixtures.
- [ ] **Linear mock**: in-memory `Client` impl for tests (scriptable issue lists + state). No network.
- [ ] **Linear writes** (`linear.SetState`, `Comment`): mutations to move a card / post a comment. Test against mock; assert exact mutation payloads.
- [ ] **state machine** (`internal/board`): pure `Next(outcome, current) -> (status, action)` map; `Promote(issue, deps) -> bool` (deps terminal → ready); guards reject illegal transitions. Test: table-driven over every (outcome × column).
- [ ] **Spawner interface** (`internal/spawn`): `Spawn(ctx, WorkerSpec) -> (RunHandle, error)` with `Kill()`, `Wait() -> (Result, error)`; `WorkerSpec{issue, lane, run_id, thread_id, workspace, env}`. Local impl execs a binary, captures stdout, parses schema-valid JSON, tracks PID.
- [ ] **local Spawner**: enforce `max_runtime` (context deadline → kill), capture exit code, redirect worker stderr to `<board>/logs/<issue>.log`. Test: success, nonzero-exit, timeout-kill, malformed-JSON output.
- [ ] **testworker** (`testworker/main.go`): reads `--issue/--lane/--run/--scenario`; emits canned schema-valid JSON per scenario (`done|needs_review|changes|blocked|continue`); scenarios `crash` (os.Exit 1), `hang` (sleep past max_runtime). Used by spawn + dispatch tests.
- [ ] **worktree lifecycle** (`internal/spawn` or `internal/store` helper): create worktree+branch off primary clone; reuse if exists (continuation); remove on terminal. Test with a temp git repo fixture.
- [ ] **dispatch tick** (`dispatcher`): one `Tick(ctx)` doing, in order — poll+upsert; reconcile (reap dead PIDs, `ReleaseStaleClaims`, crash detect, `max_runtime`); promote deps; select `ready` by (priority, created_at, identifier); apply global + per-lane caps; CAS claim; mirror Linear→running; spawn; on-exit map result via `board.Next` → Linear write + close run + events; `continue` re-spawn if under `turn_cap`, else Blocked. Test: **integration** with mock Linear + testworker for each outcome.
- [ ] **caps enforcement**: global `max_in_progress` + per-lane caps honored under a many-ready-issues integration test.
- [ ] **failure → Blocked**: crash/timeout/malformed-result/needs_input all land the card in `blocked` with a reason comment; no auto-retry. Test each path.
- [ ] **continuation cap**: repeated `continue` results stop at `turn_cap` → `blocked`. Test the boundary.
- [ ] **singleton lock** (`dispatcher`): machine-global lock (flock on a lockfile); second `clipse dispatch` refuses to start. Test.
- [ ] **daemon wiring** (`cmd/clipse` + `dispatcher`): `clipse dispatch` runs `Tick` on `poll_interval_s`; structured slog JSON; graceful shutdown (SIGINT drains, doesn't kill live workers abruptly). Manual smoke via testworker.
- [ ] **`clipse status`** (`cli`): one-shot read of SQLite snapshot → table (per-lane running/ready/blocked counts + per-issue run state). Test the render function against seeded DB.
- [ ] **`clipse tui`** (`cli/tui`): bubbletea dashboard — running / blocked / queued tables, token + runtime counters, in-place refresh polling SQLite. Manual verify; unit-test the model update fn.

### Acceptance criteria
- [ ] End-to-end against mock Linear + `testworker`: a `ready` `agent:coder` issue is claimed, spawned, and transitioned exactly per the canned outcome — with **no real LLM or Linear network**.
- [ ] **No double-claim**: concurrent-claim test passes repeatedly (race detector clean, `go test -race`).
- [ ] **Caps** hold: with many ready issues, in-flight never exceeds global or per-lane caps.
- [ ] **Recovery**: `crash` scenario → PID-death detected → card Blocked; `hang` → `max_runtime` kill → Blocked; stale heartbeat → claim released/requeued.
- [ ] **Failure policy**: every failure path lands `Blocked` with a comment; zero auto-retry; `continue` bounded by `turn_cap`.
- [ ] **Singleton**: a second dispatcher instance refuses to start.
- [ ] `clipse status` and `clipse tui` reflect live SQLite state.
- [ ] `make test` green including `go test -race ./...`; coverage on `internal/{store,board,spawn,dispatcher}` is meaningful (state machine + claim paths fully covered).

---

## Phase 2 — Coder worker (real DAC, real PR)

**Purpose:** Replace `testworker` with a real Python LangGraph Coder worker that produces actual branches, commits, and PRs. Same subprocess + typed-JSON contract, so the kernel is unchanged.

**Files / components:** `agent/src/clipse_agent/{worker.py,dac.py,graphs/coder.py,profiles/coder.py}`, checkpointer setup; kernel Spawner target switched from `testworker` to `clipse-worker`.

### Work
- [ ] **DAC API spike** (blocking, do first): confirm against `deepagents_code` source/docs — headless single-run invocation, structured/stop-reason + token capture, and **resume of a checkpointed thread by id non-interactively**. Record findings in the design doc's "to verify" section. Adjust the graph if the API differs from assumptions. *(Do not guess the API.)*
- [ ] **contract.py** consumed: worker imports generated Pydantic result model; a helper serializes it to stdout (single JSON line).
- [ ] **worker.py** entrypoint: parse `--issue/--lane/--run/--thread/--workspace`; dispatch to the lane graph by `--lane`; guarantee a schema-valid result is always emitted (even on internal error → `blocked`/`transient`).
- [ ] **dac.py**: build the DAC agent via `create_cli_agent(interactive=False, auto_approve=True, enable_shell=True, shell_allow_list=[...], model=...)` from the lane profile. Wire the LangGraph `AsyncSqliteSaver` checkpointer keyed by `thread_id`.
- [ ] **profiles/coder.py**: system prompt, toolset, skills, model, shell allow-list for the Coder lane.
- [ ] **graphs/coder.py** (LangGraph): nodes `load_context` (issue via args/Linear) → `ensure_worktree` (reuse if present) → `run_DAC` → `commit` → `push` → `open_PR` → `emit_result`. Interrupt path → `blocked(needs_input)`. Resume from checkpointer on continuation.
- [ ] **idempotent open_PR**: before creating, `gh pr view <branch>`; if a PR exists, reuse its URL; else create. Commits append to the existing branch. Test with a fake `gh` (PATH shim) simulating exists/not-exists.
- [ ] **branch/PR ↔ Linear link**: branch named from Linear so the PR auto-links; PR body references the issue.
- [ ] **kernel switch**: Spawner spawns `clipse-worker`; config points at the `agent/` entrypoint; `testworker` remains for kernel tests.
- [ ] **secrets**: `ANTHROPIC_API_KEY` + `gh` auth via `op run`/env; documented in README.
- [ ] **python tests**: graph unit tests with DAC mocked (assert node order, commit/push/PR calls, idempotency, interrupt→blocked, result schema-validity).

### Acceptance criteria
- [ ] A real Linear issue labeled `agent:coder` → dispatcher spawns the Coder worker → a branch with commits and an **auto-linked PR** appears on the configured repo.
- [ ] **Continuation**: a turn-capped run resumes the same DAC thread across turns via the checkpointer; the worktree persists prior progress.
- [ ] **Idempotency**: killing the worker after `push` but before PR-record → next turn reuses the existing PR (no duplicate). Proven by test with the `gh` shim + an integration run.
- [ ] **Blocked path**: an ambiguous issue → interrupt → card `Blocked` with a `needs_input` reason.
- [ ] Every worker exit emits a schema-valid result; dispatcher transitions correctly (verified against real runs).
- [ ] `make test` green (Go unchanged + new Python suite).

---

## Phase 3 — Reviewer, Git-operator, Scribe (full pipeline + auto-merge)

**Purpose:** Complete the four-lane pipeline and the `Review → Merging → Documentation → Done` flow, including CI-gated auto-merge and the Rework loop.

**Files / components:** `agent/src/clipse_agent/graphs/{reviewer,git_operator,scribe}.py` + matching `profiles/`; `internal/board` transitions for `rework`/`merging`/`documentation`; config for reviewer model.

### Work
- [ ] **graphs/reviewer.py**: checkout PR branch → DAC review → classify `pass`/`changes_requested` + post inline comments → `emit_result`. Advisory only.
- [ ] **profiles/reviewer.py**: optionally a distinct/stronger model; review-oriented prompt; read-mostly toolset.
- [ ] **board transitions**: `review` + `pass` → `merging`; `review` + `changes_requested` → `rework`; `rework` re-dispatches Coder; `merging` done → `documentation`; `documentation` done → `done`. Extend `board.Next` table + tests.
- [ ] **graphs/git_operator.py**: verify required CI checks green + branch protection satisfied (`gh pr checks`, protection API) → **merge** → tag if configured → cleanup worktree/branch → `emit_result`. If not mergeable → `blocked`/back to `rework`.
- [ ] **profiles/git_operator.py**: shell allow-list `[git, gh]`; merge/tag prompt + guardrails.
- [ ] **auto-merge gating**: merge happens **only** when CI + branch protection pass — the authoritative gate; reviewer `pass` is advisory input, never sufficient alone. Test the gate (mergeable vs failing-checks).
- [ ] **graphs/scribe.py**: inspect merged change + repo docs → write docs (own PR) or no-op → `emit_result`. Always-on stage.
- [ ] **profiles/scribe.py**: docs-writing prompt; toolset for docs edits + `gh`.
- [ ] **Rework loop test**: `changes_requested` → `rework` → Coder re-dispatch → back to `review` (integration with mocked lanes).
- [ ] **cleanup on terminal**: Git-operator removes worktree + local branch after merge; verify no leaked worktrees.

### Acceptance criteria
- [ ] Full happy path on a real issue: `Coder → Review →(pass)→ Merging →(CI-gated merge)→ Documentation →(docs or no-op)→ Done`.
- [ ] **Rework loop**: reviewer `changes_requested` sends the card to `Rework` and re-dispatches Coder; loop terminates on a later `pass`.
- [ ] **Merge gate**: a PR with failing/absent required checks is **not** merged even on reviewer `pass`; it routes to `Rework`/`Blocked`.
- [ ] Git-operator tags per config and cleans up the worktree/branch on terminal.
- [ ] Reviewer runs on the configured (optionally distinct) model.
- [ ] `make test` green; an end-to-end dry-run on a throwaway repo completes the full flow.

---

## Phase 4 — v2 hardening & scale (deferred)

**Purpose:** Optional extensions once the single-repo, single-machine pipeline is proven. Each is config-gated and must not regress the v1 path.

### Work
- [ ] **Orchestrator / auto-decompose**: a `Triage` intake that fans a rough issue into a linked child graph (orchestrator lane and/or auxiliary-LLM decompose), honoring Linear parent/child.
- [ ] **Multi-repo**: `repo:<name>` label → per-repo config entry + primary clone; dispatcher resolves repo per issue.
- [ ] **Remote Spawner**: SSH host-pool implementation of the `Spawner` interface (per-host caps, prefer-prior-host on retry, least-loaded) — no dispatch-loop changes.
- [ ] **Richer observability**: web dashboard + JSON API over the SQLite snapshot; OTel export of structured logs/metrics to Datadog.
- [ ] **Model/token budgets**: per-lane token budgets + spend accounting surfaced in `status`/TUI.

### Acceptance criteria
- [ ] Each feature is behind a config flag and defaults off; with all flags off, v1 behavior is byte-for-byte unchanged.
- [ ] Multi-repo and remote Spawner pass the Phase-1 kernel test suite unmodified (contract stable).
- [ ] New surfaces (dashboard/decompose) have their own test suites; `make test` green.

---

## Notes for the implementer
- Build **strictly in phase order**; Phase 1 must be fully green before Phase 2. The `testworker` + mock Linear stay in the tree permanently — they are the kernel's regression harness.
- The subprocess + typed-JSON contract is the seam: never let the kernel import Python or the worker touch the kernel DB.
- When a phase-2/3 finding contradicts the design doc (esp. the DAC API), update the design doc first, then the plan, then code.
