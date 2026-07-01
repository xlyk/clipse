# Clipse Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: use `superpowers:subagent-driven-development` (recommended) or `superpowers:executing-plans` to implement this plan task-by-task. Work items use checkbox (`- [ ]`) syntax for tracking. Within each item, follow TDD: failing test first, then implement.

**Goal:** Build Clipse — an autonomous coding-agent orchestrator that turns Linear issues into merged PRs by dispatching per-issue LangGraph/DAC workers in isolated git worktrees.

**Architecture:** Two planes. A Go **dispatcher/kernel** (single daemon) polls Linear, atomically claims work into a local SQLite runtime store, spawns per-issue worker subprocesses, monitors them, and owns every board transition off the worker's typed JSON result. A Python **worker** (LangGraph graph wrapping DAC) runs the per-issue pipeline and emits that typed result. See [design doc](../design/2026-07-01-clipse-design.md).

**Tech Stack:** Go (version per `go.mod`; currently a 1.25 floor from `modernc.org/sqlite`) (cobra CLI, bubbletea+lipgloss TUI, `log/slog` JSON logging, `modernc.org/sqlite` pure-Go driver, WAL); Python 3.13 + uv (LangGraph, `deepagents_code`, Pydantic v2, LangGraph `AsyncSqliteSaver` checkpointer); contract via JSON Schema → `go-jsonschema` + `datamodel-code-generator`; `gh` CLI for PR/merge; secrets via `op run`/env.

`go.mod` is the single source of truth for the Go version — don't restate a version elsewhere.

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
- [x] Init single Go module `github.com/xlyk/clipse` (Go version per `go.mod`); create `cmd/clipse/main.go` with a cobra root command printing version.
- [x] Create the folder skeleton: `dispatcher/`, `cli/` (+`cli/tui/`), `internal/{linear,store,board,spawn,contract,config}/`, `agent/`, `testworker/`, `schema/`, `configs/`.
- [x] Author `schema/worker-result.schema.json` — fields: `run_id`, `issue_id`, `lane`, `outcome` (enum `done|needs_review|changes_requested|blocked|continue`), `block_kind` (enum `needs_input|capability|transient|null`), `summary`, `artifacts[]`, `pr_url?`, `thread_id`, `turn_count`, `tokens{in,out}`. Include `$id`, `required`, `additionalProperties:false`.
- [x] Author `schema/board.schema.json` — enums for lanes (`coder|reviewer|git_operator|scribe`), columns (`todo|ready|running|review|merging|documentation|done|rework|blocked`), block kinds.
- [x] Wire `make codegen`: `go-jsonschema` → `internal/contract/*.go`; `datamodel-code-generator` → `agent/src/clipse_agent/contract.py`. Generated files marked do-not-edit.
- [x] Add `agent/pyproject.toml` (uv), Python 3.13, deps `langgraph`, `deepagents_code`, `pydantic>=2`; `clipse-worker` console entrypoint stub that echoes a valid empty result.
- [x] Add `Makefile` targets: `build`, `test` (Go + Python), `codegen`, `lint` (`go vet`/`ruff`), `run`.
- [x] Add `configs/clipse.example.yaml`: `repo{remote,path,base_branch}`, `poll_interval_s`, `caps{global,per_lane{...}}`, `turn_cap`, `max_runtime_s`, `lane_label_prefix: "agent:"`.
- [x] Add `.github/workflows/ci.yml`: matrix build+test Go and Python; run `make codegen` and fail on diff (drift guard).
- [x] Add `README.md` (one-paragraph overview + link to design doc) and root `.gitignore`.
- [ ] Rework `block_kind` to an optional string enum (`needs_input|capability|transient`), present iff `outcome == "blocked"`; regenerate both sides; add a Go test and a Pydantic test asserting round-trip of blocked and non-blocked results.

### Acceptance criteria
- [x] `git init` done; `make build` produces a `clipse` binary; `clipse --version` prints.
- [x] `make codegen` generates Go + Pydantic types from `schema/`; re-running is a no-op (idempotent).
- [x] CI fails if generated code is stale vs `schema/` (drift guard proven by a deliberate dirty commit in a scratch branch).
- [x] `make test` runs (green, even if only trivial tests) across both languages.
- [x] `uv run clipse-worker` emits a schema-valid JSON result on stdout.

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
- [ ] **spawn**: record `proc_started_at` (and/or pgid) in `runs` at spawn, so a later identity check can tell a live worker from a PID reused by an unrelated process after reboot.
- [ ] **startup recovery** (`dispatcher`): on start, before any claim release, for each open `runs` row — verify PID identity via `proc_started_at`/pgid, kill if alive, close the run with `error='orphaned'`, requeue the issue per the attempt cap. Test with a real spawned sleeper process: start "dispatcher recovery" against its recorded run row; assert the process dies and the issue returns to `ready`.
- [ ] **attempt cap**: `max_attempts` in config; exceeding it lands `Blocked` with a comment. Test the boundary.
- [ ] **dispatch tick** (`dispatcher`): one `Tick(ctx)` doing, in order — poll+upsert; reconcile (reap dead PIDs, `ReleaseStaleClaims`, crash detect, `max_runtime`); promote deps; select `ready` by (priority, created_at, identifier); apply global + per-lane caps; CAS claim; mirror Linear→running; spawn; on-exit map result via `board.Next` → Linear write + close run + events; `continue` re-spawn if under `turn_cap`, else Blocked. Test: **integration** with mock Linear + testworker for each outcome.
- [ ] **caps enforcement**: global `max_in_progress` + per-lane caps honored under a many-ready-issues integration test.
- [ ] **failure → Blocked**: crash/timeout/malformed-result/needs_input all land the card in `blocked` with a reason comment; no auto-retry. Test each path.
- [ ] **continuation cap**: repeated `continue` results stop at `turn_cap` → `blocked`. Test the boundary.
- [ ] **linear outbox** (`store` + `dispatcher`): enqueue the Linear mirror write in the same SQLite transaction as the transition commit (a `linear_writes` table or `pending_linear_write` column on `issues`); drain the queue each tick; retry on failure with the error logged. `SetState` is idempotent, so replays are safe. Test: mock Linear fails twice then succeeds — exactly one final `SetState`, no lost transition.
- [ ] **divergence rule** (design doc): document that for dispatcher-owned columns, SQLite wins and the outbox re-asserts; human moves are adopted per the poll-adoption rule below. *(Design doc; see "Divergence rule" under Board & state machine.)*
- [ ] **poll adoption** (`dispatcher`): on poll, if Linear disagrees with SQLite and the issue holds no active claim, adopt Linear's state (human move); if the issue holds an active claim, SQLite wins and the outbox re-asserts. Tests: (a) a card manually moved `blocked → ready` is adopted and claimed next tick; (b) a card manually moved while a claim is active is re-asserted to the SQLite state.
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
- [ ] **Restart safety**: kill the dispatcher mid-run with a live worker; restart it; the orphan is killed before any claim release, the issue is requeued exactly once, and no duplicate PR or branch results.
- [ ] With Linear down for N ticks, transitions keep committing locally and mirror correctly once Linear recovers; `clipse status` flags unmirrored issues.
- [ ] **Singleton**: a second dispatcher instance refuses to start.
- [ ] `clipse status` and `clipse tui` reflect live SQLite state.
- [ ] `make test` green including `go test -race ./...`; coverage on `internal/{store,board,spawn,dispatcher}` is meaningful (state machine + claim paths fully covered).

---

## Phase 2 — Coder worker (real DAC, real PR)

**Purpose:** Replace `testworker` with a real Python LangGraph Coder worker that produces actual branches, commits, and PRs. Same subprocess + typed-JSON contract, so the kernel is unchanged.

**Files / components:** `agent/src/clipse_agent/{worker.py,dac.py,graphs/coder.py,profiles/coder.py}`, checkpointer setup; kernel Spawner target switched from `testworker` to `clipse-worker`.

### Prerequisites
- [ ] Linear board has columns `Rework`, `Merging`, `Documentation` and labels `agent:coder|reviewer|git_operator|scribe`.
- [ ] Candidate-issue GraphQL query and branch-name auto-link verified against that board.
- [ ] `ANTHROPIC_API_KEY` available via `op run`; `gh` authenticated.
- [ ] Target (throwaway) repo with required checks + branch protection configured.
- [ ] DAC API spike findings recorded in the design doc "to verify" section.

### Work
- [ ] **DAC API spike** (blocking, do first): confirm against `deepagents_code` source/docs — headless single-run invocation, structured/stop-reason + token capture, and **resume of a checkpointed thread by id non-interactively**. Record findings in the design doc's "to verify" section. Adjust the graph if the API differs from assumptions. *(Do not guess the API.)*
- [ ] **contract.py** consumed: worker imports generated Pydantic result model; a helper serializes it to stdout (single JSON line).
- [ ] **worker.py** entrypoint: parse `--issue/--lane/--run/--thread/--workspace`; dispatch to the lane graph by `--lane`; guarantee a schema-valid result is always emitted (even on internal error → `blocked`/`transient`).
- [ ] **dac.py**: build the DAC agent via `create_cli_agent(interactive=False, auto_approve=True, enable_shell=True, shell_allow_list=[...], model=...)` from the lane profile. Wire the LangGraph `AsyncSqliteSaver` checkpointer keyed by `thread_id`.
- [ ] **checkpointer path**: one checkpointer database per issue at `<board>/checkpoints/<issue_id>.db` (outside the worktree — no gitignore entry needed). Worker derives the path from an explicit `--checkpoint-db` arg (the kernel owns paths, not worker-side convention). Test: two issues run concurrently, distinct files, no cross-talk.
- [ ] **checkpointer cleanup**: terminal-state cleanup removes the checkpoint file along with the worktree. Test: after `done`, neither worktree nor checkpoint file remains.
- [ ] **token ceiling**: enforce `max_tokens_per_run` (config, passed via env/arg) in the worker; the worker tracks usage from DAC callbacks and aborts over budget with `outcome=blocked, block_kind=capability` and a summary naming the spend. Test with a mocked DAC token stream crossing the limit.
- [ ] **env scrubbing** (`spawn`): the Spawner passes an explicit env allow-list per lane, not the dispatcher's environment — so a worker never sees `LINEAR_API_KEY` or other kernel-only secrets. Test: worker env contains only the allow-listed keys.
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

**Files / components:** `agent/src/clipse_agent/graphs/{reviewer,scribe}.py` + matching `profiles/`; `internal/gitops` (merge/tag/cleanup, deterministic Go — replaces a `git_operator` graph/profile); `internal/board` transitions for `rework`/`merging`/`documentation`; config for reviewer model.

### Work
- [ ] **graphs/reviewer.py**: checkout PR branch → DAC review → classify `pass`/`changes_requested` + post inline comments → `emit_result`. Advisory only.
- [ ] **profiles/reviewer.py**: optionally a distinct/stronger model; review-oriented prompt; read-mostly toolset.
- [ ] **board transitions**: `review` + `pass` → `merging`; `review` + `changes_requested` → `rework`; `rework` re-dispatches Coder; `merging` done → `documentation`; `documentation` done → `done`. Extend `board.Next` table + tests.
- [ ] **rework cap** (`store` + `board` + `dispatcher`): add `rework_cap` (config, default 3) and `issues.rework_count`, reset on `done`; count each review↔rework cycle per issue; exceeding the cap lands `Blocked` with a comment linking the PR and the last review. Table-driven test over the boundary (cap, cap+1).
- [ ] **gitops** (`internal/gitops`, deterministic Go — replaces the Python `git_operator` graph/profile): check required CI checks + branch protection (`gh pr checks`, protection API) → merge → optional tag → remove worktree + local branch. Test against a fake `gh` PATH shim: mergeable, failing-checks, absent-checks, protection-unsatisfied.
- [ ] **board wiring**: `merging` cards route to `internal/gitops` instead of a spawned worker; outcomes map exactly as the lane's results did (merged → `documentation`; not mergeable → `rework`/`blocked`).
- [ ] **decision log**: amend J — "Git-operator lane executes as deterministic kernel code; the lane label is board semantics only." *(Already folded into the design doc's decision log, row J.)*
- [ ] **stale-base handling** (`internal/gitops`): when a PR is blocked only by a stale base, update it (`gh pr update-branch`, or rebase-and-push) and re-check; on conflict, route to `Rework` with a comment naming the conflicting files. Test both paths with the `gh` shim.
- [ ] **auto-merge gating**: merge happens **only** when CI + branch protection pass — the authoritative gate; reviewer `pass` is advisory input, never sufficient alone. Test the gate (mergeable vs failing-checks).
- [ ] **graphs/scribe.py**: inspect merged change + repo docs → write docs (own PR) or no-op → `emit_result`. Always-on stage.
- [ ] **profiles/scribe.py**: docs-writing prompt; toolset for docs edits + `gh`.
- [ ] **Rework loop test**: `changes_requested` → `rework` → Coder re-dispatch → back to `review` (integration with mocked lanes).
- [ ] **cleanup on terminal**: `internal/gitops` removes worktree + local branch after merge; verify no leaked worktrees.

### Acceptance criteria
- [ ] Full happy path on a real issue: `Coder → Review →(pass)→ Merging →(CI-gated merge)→ Documentation →(docs or no-op)→ Done`.
- [ ] **Rework loop**: reviewer `changes_requested` sends the card to `Rework` and re-dispatches Coder; loop terminates on a later `pass`.
- [ ] **Rework cap**: a permanently-disagreeing reviewer (mocked) drives the issue to `Blocked` after exactly `rework_cap` cycles — never an infinite loop.
- [ ] **Merge gate**: a PR with failing/absent required checks is **not** merged even on reviewer `pass`; it routes to `Rework`/`Blocked`.
- [ ] **Stale-base recovery**: two issues merge back-to-back — the second PR is auto-updated after the first merge and lands without human help.
- [ ] Git-operator tags per config and cleans up the worktree/branch on terminal.
- [ ] Reviewer runs on the configured (optionally distinct) model.
- [ ] `make test` green; an end-to-end dry-run on a throwaway repo completes the full flow.

---

## Phase 4 — v2 hardening & scale (deferred)

**Purpose:** Optional extensions once the single-repo, single-machine pipeline is proven. Each is config-gated and must not regress the v1 path.

Note (B2): the worker-side token *ceiling* (`max_tokens_per_run`) is pulled
forward into Phase 2, not deferred here. Phase 4 keeps kernel-side accounting
and per-lane budgets.

### Work
- [ ] **Orchestrator / auto-decompose**: a `Triage` intake that fans a rough issue into a linked child graph (orchestrator lane and/or auxiliary-LLM decompose), honoring Linear parent/child.
  - Scope: one new `Triage` lane + graph; no changes to the Phase-1 state machine.
  - Risk: decomposition quality is unverified — a bad split creates more rework than it saves.
  - Graduation trigger: manually decomposing issues costs more than one hour/week.
- [ ] **Multi-repo**: `repo:<name>` label → per-repo config entry + primary clone; dispatcher resolves repo per issue.
  - Scope: config schema + repo-resolution lookup; no change to per-issue worktree lifecycle.
  - Risk: per-repo cap/lane config drifting out of sync across repos.
  - Graduation trigger: a second repo actually needs lanes.
- [ ] **Remote Spawner**: SSH host-pool implementation of the `Spawner` interface (per-host caps, prefer-prior-host on retry, least-loaded) — no dispatch-loop changes.
  - Scope: new `Spawner` impl behind the existing interface; local Spawner remains default.
  - Risk: network-partitioned hosts look identical to crashed workers, muddying crash recovery.
  - Graduation trigger: local caps saturate the machine (sustained cap-bound queue).
- [ ] **Richer observability**: web dashboard + JSON API over the SQLite snapshot; OTel export of structured logs/metrics to Datadog.
  - Scope: read-only dashboard/API layered on the existing SQLite snapshot; no new write paths.
  - Risk: a second "view of truth" that drifts from the TUI/`status` if not sourced from the same snapshot query.
  - Graduation trigger: TUI insufficient for >1 observer, or debugging needs history Datadog would hold.
- [ ] **Model/token budgets**: per-lane token budgets + spend accounting surfaced in `status`/TUI.
  - Scope: kernel-side accounting + per-lane budget config; builds on the Phase-2 worker-side ceiling (B2), doesn't replace it.
  - Risk: budget enforcement racing with in-flight runs if accounting isn't transactional with the run's close.
  - Graduation trigger: monthly spend exceeds a set dollar threshold.

### Acceptance criteria
- [ ] Each feature is behind a config flag and defaults off; with all flags off, v1 behavior is byte-for-byte unchanged.
- [ ] Multi-repo and remote Spawner pass the Phase-1 kernel test suite unmodified (contract stable).
- [ ] New surfaces (dashboard/decompose) have their own test suites; `make test` green.

---

## Notes for the implementer
- Build **strictly in phase order**; Phase 1 must be fully green before Phase 2. The `testworker` + mock Linear stay in the tree permanently — they are the kernel's regression harness.
- The subprocess + typed-JSON contract is the seam: never let the kernel import Python or the worker touch the kernel DB.
- When a phase-2/3 finding contradicts the design doc (esp. the DAC API), update the design doc first, then the plan, then code.
