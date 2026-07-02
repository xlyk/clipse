# Clipse — agent & contributor guide

Clipse turns Linear issues into merged PRs. A Go **dispatcher/kernel** polls Linear, atomically claims work into local SQLite, spawns per-issue Python **LangGraph + Deep Agents Code (DAC)** worker subprocesses in git worktrees, and owns every board transition off the worker's typed JSON result. The kernel is deterministic and LLM-free; the LLM lives only in the worker.

Full rationale + decision log: `docs/design/2026-07-01-clipse-design.md`. Phased work + acceptance criteria: `docs/plans/2026-07-01-clipse-implementation-plan.md`. Applied code-review amendments: `docs/plans/2026-07-01-plan-amendments.md`.

## Status

- **Phase 0 (scaffold + contract) and Phase 1 (zero-LLM Go kernel) are complete** — built test-first against a fake worker (`testworker`) and a mocked Linear, `go test -race ./...` clean.
- **Phase 2 (real DAC coder worker) and Phase 3 (reviewer / git-operator / scribe lanes, auto-merge) are not started and are blocked** pending: a DAC API spike (verify `deepagents_code` headless run, structured result, and non-interactive checkpointed-thread resume against source — do not guess the API), `ANTHROPIC_API_KEY`, `gh` auth, and a real Linear board (custom columns + `agent:<lane>` labels) with a target repo.

## Build, test, run

`make` from the repo root:

- `make build` — compile `./bin/clipse`.
- `make test` — `test-go` + `test-py` (Go suite + `agent/` pytest). The gate.
- `make lint` — `go vet`, a `gofmt` check, and `ruff` on `agent/`.
- `make codegen` — regenerate the Go + Pydantic contract from `schema/` (idempotent; CI fails on drift).
- `make run` — `go run ./cmd/clipse`.

Binary subcommands: `clipse dispatch` (the daemon), `clipse status` (one-shot SQLite snapshot table), `clipse tui` (live dashboard). Kernel tests need **no** network or LLM.

## Layout

- `cmd/clipse` — thin entrypoint → `cli.NewRootCmd()`.
- `cli/` — cobra subcommands (`dispatch`, `status`, `tui`); `cli/tui/` is the bubbletea model.
- `dispatcher/` — the daemon and the deterministic `Tick` loop.
- `internal/config` — typed `clipse.yaml` load + validation + defaults.
- `internal/store` — SQLite kernel (issues / runs / events / linear_writes), CAS claim, outbox.
- `internal/board` — pure state machine (`Next`, `Promote`).
- `internal/spawn` — `Spawner` + local impl, `testworker` process control, git worktree lifecycle, orphan reaping.
- `internal/linear` — GraphQL `Client`, normalize, in-memory mock, writes.
- `internal/contract` — **generated** Go types (do not edit).
- `schema/` — `worker-result.schema.json` + `board.schema.json`, the shared source of truth.
- `agent/` — uv Python project; `clipse-worker` entrypoint; `contract.py` is **generated**.
- `testworker/` — Go fake worker emitting canned schema-valid JSON (kernel regression harness; stays in the tree permanently).
- `configs/clipse.example.yaml` — config shape.

## Kernel invariants (do not violate)

- **`board_status='running'` is entered ONLY via the CAS claim** (`store.ClaimReady`, a `BEGIN IMMEDIATE` compare-and-swap on `board_status='ready' AND claim_lock IS NULL`). Never write `running` directly.
- **SQLite is runtime truth; Linear is task intent.** The dispatcher is the only *automated* writer of board state. A re-poll never clobbers a dispatcher-owned `board_status` (`UpsertIssue` preserves it on conflict); humans may move cards, and the poll adopts the move only when the issue holds no active claim (else SQLite wins and the outbox re-asserts).
- **Linear is written ONLY through the outbox.** Transitions enqueue a `linear_writes` row in the *same* transaction as the state change (`store.Transition`); `dispatcher.drainOutbox` mirrors pending rows each tick and retries on failure. An outage never loses or duplicates a transition. `clipse status` flags issues with pending (unmirrored) writes.
- **Failures park in `blocked` with a reason comment — no auto-retry.** Continuation is bounded by `turn_cap`; a dispatcher-restart orphan is requeued under `max_attempts` (then blocked), and its process is killed with a PID-identity guard *before* any stale-claim release.
- **`Tick` is single-goroutine and race-free.** Each spawn starts one goroutine that blocks on `RunHandle.Wait()` and sends the result on a buffered channel; `Tick` drains it non-blocking. The in-flight map is touched only by the `Tick` goroutine. The whole package passes `go test -race`.
- **One dispatcher per machine** (flock singleton). Graceful shutdown must not kill live workers: the worker's timeout context is rooted at `context.WithoutCancel(...)`, so only `max_runtime_s` (never SIGINT) kills a worker.
- **Bare lane everywhere in the store.** `issues.lane_label` and `runs.lane` hold the bare lane (`coder` / `reviewer` / `git_operator` / `scribe`), matching the contract `Lane` enum and the config `per_lane` keys. The `agent:` prefix lives only in Linear-label parsing.
- **`block_kind` is present iff `outcome == "blocked"`** (optional string enum, omitted otherwise).

## Contract & codegen

`schema/*.schema.json` is authoritative. `make codegen` regenerates `internal/contract/contract.go` (via `github.com/atombender/go-jsonschema`) and `agent/src/clipse_agent/contract.py` (via `datamodel-code-generator`), both carrying a do-not-edit banner and pinned tool versions. Edit the schema, then regenerate — never hand-edit generated files. CI runs `make codegen` and fails on any diff.

## Conventions

- **TDD**: failing test first, then the minimal implementation. `make test` is the gate.
- **Go**: check and wrap every error (`fmt.Errorf("...: %w", err)`); no swallowed errors. Runtime/daemon logs use `log/slog` JSON only — no `fmt.Println` (a CLI command *result*, e.g. a `status` table, is fine on stdout). Table-driven tests; standard-library `testing` only (no testify). Interfaces at the consumption site.
- **Dependencies** (ask before adding anything else): Go — `spf13/cobra`, `charmbracelet/bubbletea` + `lipgloss` + `bubbles` (TUI widgets: viewport/progress/help/key), `modernc.org/sqlite` (pure-Go, WAL, `_txlock=immediate`), `gopkg.in/yaml.v3`; otherwise stdlib. Python — `pydantic>=2`, `langgraph`, `deepagents-code`; dev `pytest` + `ruff`.
- **Commits**: Conventional Commits, casual/lowercase, no trailing period, no AI/Claude signature. One concern per commit. Never `git add -A`/`git add .` (an untracked `.superpowers/` SDD ledger must never be committed). Open PRs as drafts; push with `--force-with-lease`, never `--no-verify`.
- **Testing philosophy**: the kernel is proven against `testworker` (drive scenarios via the `TESTWORKER_SCENARIO` env in `WorkerSpec.Env`) + `linear.MockClient`. Zero LLM, zero real network (only `httptest` loopback). Detect worker timeout via `errors.Is(res.Err, context.DeadlineExceeded)`, not exit code.

## Open follow-ups (Phase 2–3)

- **Cross-lane claiming**: `board.Next` expects reviewer/git-operator/scribe outcomes to resolve while the issue sits at `review`/`merging`/`documentation`, but `ClaimReady` only claims from `ready`→`running`. Phase 1 is coder-only, so this is unexercised; Phase 3 must choose handoff-spawn (off the transition action tag) vs. generalizing `ClaimReady` to a per-lane entry column.
- **Worktree cleanup**: `dispatcher.Workspacer.Remove` exists but is intentionally unwired — wire it on the terminal (`done`/after-merge) transition in Phase 3 (design decision F) and assert no leaked worktrees.
- **Linear `SetState`** — ✅ done (Phase 2): the HTTP client resolves `contract.Column` → per-team workflow-state id (fetch the team's states, map by name via the inverse of `statusFromWorkflowName`, cached) and scopes the candidate-issue query to the configured team.
- **Git-operator is deterministic Go** (`internal/gitops`), not a DAC lane (decision O / amended J) — merge/tag/cleanup + CI-and-branch-protection gating live in the kernel in Phase 3.
- **Worker interrupt-resume (HITL answer)**: the coder graph + `dac.py` support `Command(resume=...)` (unit-tested), but no live producer feeds a human's answer back — a `blocked(needs_input)` issue that is requeued is re-run as a fresh `task_text` turn on the same thread, not resumed with an answer to the pending interrupt. Design the human-answer → resume channel (e.g. a `--resume-payload` sourced from a Linear comment) in Phase 2/3. (The turn-cap `continue` path is unused: the DAC agent runs to completion within one worker turn, emitting `needs_review`/`blocked`.)
