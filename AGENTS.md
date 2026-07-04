# Clipse — agent & contributor guide

Clipse turns Linear issues into merged PRs. A Go **dispatcher/kernel** polls Linear, atomically claims work into local SQLite, spawns per-issue Python **LangGraph + Deep Agents Code (DAC)** worker subprocesses in git worktrees, and owns every board transition off the worker's typed JSON result. The kernel is deterministic and LLM-free; the LLM lives only in the worker.

Full rationale + decision log: `docs/design/2026-07-01-clipse-design.md`. Phased work + acceptance criteria: `docs/plans/2026-07-01-clipse-implementation-plan.md`. Applied code-review amendments: `docs/plans/2026-07-01-plan-amendments.md`.

## Status

- **Phase 0 (scaffold + contract) and Phase 1 (zero-LLM Go kernel) are complete** — built test-first against a fake worker (`testworker`) and a mocked Linear, `go test -race ./...` clean.
- **Phase 2 (real DAC coder worker) and Phase 3 (reviewer / git-operator lanes, auto-merge; documentation folded into the coder graph) are not started and are blocked** pending: a DAC API spike (verify `deepagents_code` headless run, structured result, and non-interactive checkpointed-thread resume against source — do not guess the API), `ANTHROPIC_API_KEY`, `gh` auth, a real Linear board (custom columns + `agent:<lane>` labels) with a target repo, and — for any lane configured on the `openai_codex` model provider — a one-time interactive ChatGPT sign-in on the dispatcher host, run as the same OS user the dispatcher runs as (`uv --project agent run dcode` → `/auth` → `openai_codex`; token lands at `~/.deepagents/.state/chatgpt-auth.json`, reachable because `HOME` is already in the env allow-list; auto-refreshes after).

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
- **Transient failures auto-recover (bounded); everything else parks in `blocked`.** A transient failure — a worker `block_kind=transient`, a run-level crash / malformed-result / timeout, or a spawn/workspace failure — is deterministically re-queued to its release column (`store.ReleaseTargetColumn`) under a bounded budget: `recover_attempts < recover_cap`, each retry delayed by `recover_backoff_s` via `issues.blocked_until` (a card inside its backoff window is invisible to every claim/peek — the anti-hot-loop guard). `recover_attempts` resets to 0 when the card next advances normally. Once the budget is spent — or for a non-transient block (`capability`/`needs_input`), a `rework_cap` exhaustion, an illegal transition, or an orphan past `max_attempts` — the card parks in `blocked` with a reason comment and is never auto-requeued (a human must move it). `dispatcher.parkOrRetry` is the single retry-vs-park decision point; the recovery re-queue goes through `store.Transition` (outbox-mirrored) like any other transition. Continuation is bounded by `turn_cap`; a dispatcher-restart orphan is requeued under `max_attempts` (then blocked), and its process is killed with a PID-identity guard *before* any stale-claim release.
- **`Tick` is single-goroutine and race-free.** Each spawn starts one goroutine that blocks on `RunHandle.Wait()` and sends the result on a buffered channel; `Tick` drains it non-blocking. The in-flight map is touched only by the `Tick` goroutine. The whole package passes `go test -race`.
- **One dispatcher per machine** (flock singleton). Graceful shutdown must not kill live workers: the worker's timeout context is rooted at `context.WithoutCancel(...)`, so only `max_runtime_s` (never SIGINT) kills a worker.
- **Bare lane everywhere in the store.** `issues.lane_label` and `runs.lane` hold the bare lane (`coder` / `reviewer` / `git_operator`), matching the contract `Lane` enum and the config `per_lane` keys. The `agent:` prefix lives only in Linear-label parsing. (Documentation is a step *inside* the coder graph, not its own lane — see below.)
- **`block_kind` is present iff `outcome == "blocked"`** (optional string enum, omitted otherwise).

## Contract & codegen

`schema/*.schema.json` is authoritative. `make codegen` regenerates `internal/contract/contract.go` (via `github.com/atombender/go-jsonschema`) and `agent/src/clipse_agent/contract.py` (via `datamodel-code-generator`), both carrying a do-not-edit banner and pinned tool versions. Edit the schema, then regenerate — never hand-edit generated files. CI runs `make codegen` and fails on any diff.

## Conventions

- **TDD**: failing test first, then the minimal implementation. `make test` is the gate.
- **Go**: check and wrap every error (`fmt.Errorf("...: %w", err)`); no swallowed errors. Runtime/daemon logs use `log/slog` JSON only — no `fmt.Println` (a CLI command *result*, e.g. a `status` table, is fine on stdout). Table-driven tests; standard-library `testing` only (no testify). Interfaces at the consumption site.
- **Dependencies** (ask before adding anything else): Go — `spf13/cobra`, `charmbracelet/bubbletea` + `lipgloss` + `bubbles` (TUI widgets: viewport/progress/help/key), `modernc.org/sqlite` (pure-Go, WAL, `_txlock=immediate`), `gopkg.in/yaml.v3`; otherwise stdlib. Python — `pydantic>=2`, `langgraph`, `deepagents-code`; dev `pytest` + `ruff`.
- **Commits**: Conventional Commits, casual/lowercase, no trailing period, no AI/Claude signature. One concern per commit. Never `git add -A`/`git add .` (an untracked `.superpowers/` SDD ledger must never be committed). Open PRs as drafts; push with `--force-with-lease`, never `--no-verify`.
- **Testing philosophy**: the kernel is proven against `testworker` (drive scenarios via the `TESTWORKER_SCENARIO` env in `WorkerSpec.Env`) + `linear.MockClient`. Zero LLM, zero real network (only `httptest` loopback). Detect worker timeout via `errors.Is(res.Err, context.DeadlineExceeded)`, not exit code.
- **Model config**: per-lane models live in `clipse.yaml`'s `models:` block, keyed by profile identity — `coder` / `coder_docs` / `reviewer`, not `per_lane` (concurrency caps) and not `git_operator` (that lane is deterministic Go, never a DAC agent). Each key defaults independently to the prior hardcoded string (coder/coder_docs `anthropic:claude-sonnet-4-6`, reviewer `anthropic:claude-opus-4-6`), so an unconfigured deploy is unchanged. `internal/config` validates `provider:model` shape only (non-empty on both sides of the colon) — no model vocabulary, keeping the kernel LLM-free.
- **Per-lane `model_params`**: an optional `model_params:` block alongside `models:`, keyed the same way (`coder` / `coder_docs` / `reviewer`; omit a lane and it gets no overrides). Each key is an arbitrary map threaded opaque end to end — Go `config.ModelParams` → a JSON string on `WorkerSpec` → `--model-params`/`--docs-model-params` argv → the Python profile's `model_params` dict → DAC's `create_model(extra_kwargs=...)` — with no semantic validation anywhere, keeping the kernel LLM-free.
- **`model_params` precedence and effort**: `~/.deepagents/config.toml`'s provider params (machine-global, e.g. `[models.providers.openai_codex.params]` with `reasoning_effort = "high"`) are the base; a lane's `clipse.yaml` `model_params` overrides them through `extra_kwargs`. A lane with no `model_params` inherits config.toml. Codex expresses effort as the flat `reasoning_effort` scalar (`minimal`/`low`/`medium`/`high`); anthropic has no "effort" knob — use `thinking`/`max_tokens` instead. Example: `model_params: { coder: { reasoning_effort: high }, coder_docs: { reasoning_effort: low } }`.
- **`create_model` routing**: a bare `openai_codex:*` spec string can't go straight into `create_cli_agent` — it routes through LangChain's `init_chat_model`, which raises `ValueError` on the DAC-only `openai_codex` provider (never registered with LangChain). `dac.build_coder_agent` pre-builds a `BaseChatModel` via `deepagents_code.config.create_model(...)` — passing the object instead of the spec string — whenever the provider is `openai_codex` *or* the profile carries `model_params` (only `create_model` accepts `extra_kwargs`); a paramless non-codex lane keeps the plain-string path untouched.

## Open follow-ups (Phase 2–3)

- **Documentation is a coder-graph step, not a lane** (reversed design decision D3): `graphs/coder.py`'s `run_docs` node drives a best-effort docs DAC turn (restricted `get_coder_docs_profile`) right before the PR is opened, so docs ride the same commit/PR as the code and get reviewed. A merged PR goes `merging → done` directly. This removed the `documentation` column, the `scribe` lane, and the `-docs` branch/worktree apparatus (`EnsureDocs`/`EnsureDocsWorktree`) — and with it the non-fast-forward push bug and the `-docs` worktree leak.
- **Cross-lane claiming**: `board.Next` resolves reviewer outcomes while the issue sits at `review` and git-operator outcomes at `merging`; `store.ClaimColumn` claims per-column (`ClaimReady` still handles only `ready`→`running` for the coder). Shipped (decision P).
- **Worktree cleanup**: the coder worktree + branch are removed by `internal/gitops` after a successful merge (decision F). The dead `Workspacer.Remove` was deleted. Cleanup on a non-merged terminal state (`cancelled`) is still unwired.
- **Rework feedback carries the reviewer's rollup, not its inline findings**: `store.LatestReworkFeedback` → `CLIPSE_REVIEW_FEEDBACK` threads the newest `changes_requested` run's `summary` into the coder's rework re-run (fixed the byte-identical dead-loop). But that summary can go vague ("diff unchanged, same findings") instead of restating the actionable per-line findings, so convergence can hinge on the coder recovering specifics from the PR/context. Thread the reviewer's inline PR comments (or make `graphs/reviewer.py` always restate findings in its summary) so a rework turn always gets the actual asks.
- **Linear `SetState`** — ✅ done (Phase 2): the HTTP client resolves `contract.Column` → per-team workflow-state id (fetch the team's states, map by name via the inverse of `statusFromWorkflowName`, cached) and scopes the candidate-issue query to the configured team.
- **Git-operator is deterministic Go** (`internal/gitops`), not a DAC lane (decision O / amended J) — merge/tag/cleanup + CI-and-branch-protection gating live in the kernel in Phase 3.
- **Worker interrupt-resume (HITL answer)**: the coder graph + `dac.py` support `Command(resume=...)` (unit-tested), but no live producer feeds a human's answer back — a `blocked(needs_input)` issue that is requeued is re-run as a fresh `task_text` turn on the same thread, not resumed with an answer to the pending interrupt. Design the human-answer → resume channel (e.g. a `--resume-payload` sourced from a Linear comment) in Phase 2/3. (The turn-cap `continue` path is unused: the DAC agent runs to completion within one worker turn, emitting `needs_review`/`blocked`.)
- **openai_codex OAuth-token reach (accepted risk)**: `~/.deepagents/.state/chatgpt-auth.json` sits under the `HOME` the coder worker already inherits, reachable by the coder lane's own allow-listed shell tools (`cat`/`find`/`grep`/`rg`) — a crafted Linear issue body could steer the agent into reading or exfiltrating it. Unlike the env-var-only `ANTHROPIC_API_KEY`/scoped `gh` token beside it, this is a persistent, account-level credential — a broader blast radius. Accepted for this project's single-tenant, personal-use posture (see `docs/design/2026-07-03-configurable-per-lane-models.md`'s Risks); future hardening: keep the token outside the coder's readable path.
