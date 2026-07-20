# AGENTS.md — Clipse agent & contributor guide

Clipse turns Linear issues into merged PRs. A Go **dispatcher/kernel** polls Linear, atomically claims work into local SQLite, spawns per-issue Python **LangGraph + Deep Agents Code (DAC)** workers against the configured agent backend, and owns every board transition from the worker's typed JSON result. The kernel is deterministic and LLM-free; the LLM lives only in the worker.

Full rationale + decision log: `docs/design/2026-07-01-clipse-design.md`. Phased work + acceptance criteria: `docs/plans/2026-07-01-clipse-implementation-plan.md`. Applied code-review amendments: `docs/plans/2026-07-01-plan-amendments.md`.

## Status

- **Phase 0 (scaffold + contract) and Phase 1 (zero-LLM Go kernel) are complete** — built test-first against a fake worker (`testworker`) and a mocked Linear, `go test -race ./...` clean.
- **Phase 2 (real DAC coder worker) and Phase 3 (reviewer / git-operator lanes, auto-merge; documentation folded into the coder graph) are complete**: the coder and reviewer lanes run real DAC turns, `internal/gitops` owns the CI-and-branch-protection merge gate, per-lane models and the `shell_allow_list` policy are configurable, and the live eval suite (`make eval`) exercises both lanes end to end against the real model + `gh`. The recommended Daytona backend runs DAC filesystem/shell tools in remote sandboxes; local worktrees remain the compatibility default when `agent_backend` is absent. Still pending for any lane configured on the `openai_codex` model provider: a one-time interactive ChatGPT sign-in on the dispatcher host, run as the same OS user the dispatcher runs as (`uv --project agent run dcode` → `/auth` → `openai_codex`; token lands at `~/.deepagents/.state/chatgpt-auth.json`; auto-refreshes after).

## Build, test, run

`make` from the repo root:

- `make build` — compile `./bin/clipse`.
- `make test` — `test-go` + `test-py` (Go suite + `agent/` pytest). The gate.
- `make lint` — `go vet`, a `gofmt` check, and `ruff` on `agent/`.
- `make codegen` — regenerate the Go + Pydantic contract from `schema/` (idempotent; CI fails on drift).
- `make run` — `go run ./cmd/clipse`.
- `make eval` — live-model evals for the coder/coder_docs/reviewer
  agents (`agent/evals/`, pytest, real git + fake `gh` shim; needs
  `ANTHROPIC_API_KEY`, costs tokens, never runs in `make test`/CI). Each case
  pins a clipse-specific behavior or a production incident; DAC engine
  mechanics are upstream's job (`langchain-ai/deepagents` `libs/evals` — run
  that suite when bumping the DAC pin). Model override:
  `CLIPSE_EVAL_MODEL=openai_codex:gpt-5-codex make eval`. See
  `agent/evals/README.md`.
- `make smoke-daytona-backend` — opt-in production-path Daytona smoke; opens a draft PR, runs coder/reviewer DAC turns, never merges, and cleans all live resources.

Binary subcommands: `clipse dispatch` (the daemon), `clipse status` (one-shot SQLite snapshot table), `clipse tui` (live dashboard), `clipse board plan|apply` (bootstrap a Linear board from a `board.yaml` spec — see Board bootstrap below). Kernel tests need **no** network or LLM.

<!-- managed:readme-agents-doc:section=STYLE:BEGIN -->
## Code style

- Go: wrap errors, use `log/slog` JSON for runtime logs, and keep stdout for CLI results.
- Tests: standard-library `testing`; no testify. Put interfaces at the consumer.
- Python: `agent/` uv plus `ruff`; change generated `contract.py` only through `make codegen`.
<!-- managed:readme-agents-doc:section=STYLE:END -->

<!-- managed:readme-agents-doc:section=TESTING:BEGIN -->
## Testing

- Gate: `make test` (`go test -race ./...` plus `cd agent && uv run pytest`).
- Go focus: `go test ./dispatcher -run TestName` or the changed package.
- Python focus: `cd agent && uv run pytest tests/test_file.py::test_name`.
- Live evals: `make eval`; costs tokens and stays out of normal CI.
<!-- managed:readme-agents-doc:section=TESTING:END -->

<!-- managed:readme-agents-doc:section=SECURITY:BEGIN -->
## Security & secrets

- `LINEAR_API_KEY` is kernel-only; `internal/config` rejects it in `env_allowlist`.
- Workers may inherit only allow-listed env such as `ANTHROPIC_API_KEY`, `GH_TOKEN`/`GITHUB_TOKEN`, `HOME`, and `PATH`.
- Daytona controller actions may inherit `DAYTONA_API_KEY`; local workers never do. Host `gh` credentials pass only as Daytona SDK Git method arguments, never through sandbox env, Git config, prompts, transcripts, results, or argv.
- `openai_codex:*` stores OAuth under inherited `HOME`; treat `chatgpt-auth.json` as an account credential.
- Default `shell_allow_list: all` means unrestricted shell. Narrow it for lower-trust issues.
<!-- managed:readme-agents-doc:section=SECURITY:END -->

<!-- managed:readme-agents-doc:section=PR_CONVENTIONS:BEGIN -->
## PR conventions

- Conventional Commits; lowercase/casual subject; no trailing period or AI/Claude signature.
- One concern per commit. Never `git add -A` or `git add .`; stage exact files.
- Draft PRs by default. Push rewrites with `--force-with-lease`; never `--no-verify`.
- Required checks on `main`: `go`, `python`, `codegen-drift`.
<!-- managed:readme-agents-doc:section=PR_CONVENTIONS:END -->

<!-- managed:readme-agents-doc:section=GOTCHAS:BEGIN -->
## Gotchas

- **SQLite wins over Linear drift** — re-polling Linear must not clobber dispatcher-owned board state; use the outbox path for automated writes.
- **`running` is claim-only** — only the CAS claim may enter `running`; direct status writes break recovery and Linear reconciliation.
- **Generated contracts are off-limits** — edit `schema/`, then run `make codegen`; CI fails on codegen drift.
- **Docs are inside the coder graph** — there is no `documentation` column, `scribe` lane, or `-docs` worktree.
- **`max_tokens_per_run` is per DAC round** — it is a context guard after auto-compaction, not a cumulative spend cap.
- **Codex auth is user-sensitive** — `/auth` must run as the dispatcher OS user so worker `HOME` finds the token.
- **Daytona never falls back to local** — lifecycle/preflight failures use the normal typed block/retry path. Do not add an automatic host-worktree fallback.
- **Workspace cleanup is durable** — terminal coder workspaces and every reviewer workspace are queued in SQLite; the dispatcher retries cleanup and reconciles provider inventory on startup.
<!-- managed:readme-agents-doc:section=GOTCHAS:END -->

## Layout

- `cmd/clipse` — thin entrypoint → `cli.NewRootCmd()`.
- `cli/` — cobra subcommands (`dispatch`, `status`, `tui`, `board`); `cli/tui/` is the bubbletea model.
- `dispatcher/` — the daemon and the deterministic `Tick` loop.
- `internal/config` — typed `clipse.yaml` load + validation + defaults.
- `internal/store` — SQLite kernel (issues / runs / events / linear_writes), CAS claim, outbox.
- `internal/board` — pure state machine (`Next`, `Promote`).
- `internal/boardspec` — pure board-bootstrap: parse/validate `board.yaml`, the ref+sha content marker, and the create/update/skip reconciliation plan (no network, no LLM). Distinct from `internal/board` (the kernel state machine).
- `internal/linear/bootstrap` — the Linear mutation client (`issueCreate`/`issueUpdate`/`issueRelationCreate`/label ensure) that executes a `boardspec` plan. Walled off from `internal/linear.HTTPClient` so the dispatcher can never create or delete issues.
- `internal/spawn` — `Spawner` + local impl, `testworker` process control, git worktree lifecycle, orphan reaping.
- `internal/backend` — provider-neutral lifecycle manager and command-backed Daytona protocol.
- `internal/linear` — GraphQL `Client`, normalize, in-memory mock, writes.
- `internal/contract` — **generated** Go types (do not edit).
- `schema/` — `worker-result.schema.json` + `board.schema.json`, the shared source of truth.
- `agent/` — uv Python project; `clipse-worker` entrypoint; `contract.py` is **generated**.
- `agent/src/clipse_agent/backends/` — local/Daytona sessions, lifecycle contracts, and trusted host GitHub helpers.
- `testworker/` — Go fake worker emitting canned schema-valid JSON (kernel regression harness; stays in the tree permanently).
- `configs/clipse.example.yaml` — config shape; `configs/board.example.yaml` — board-spec shape (see `schema/board-spec.schema.json`, a reference-only schema, not codegen'd).
- `skills/clipse-board-bootstrap` — repo-versioned skill: turns a prose project plan into a schema-valid `board.yaml` for `clipse board apply`. The LLM decomposition lives here, never in Go.

## Board bootstrap

Starting clipse on a new project needs a Linear board: issues, a `blocked-by`
dependency DAG, `agent:<lane>` labels, and a start state. That is now a
first-class flow, not a smoke-script side effect:

1. The `clipse-board-bootstrap` skill turns a prose plan into a `board.yaml`
   (+ `body_file` markdowns) — the only LLM step, kept out of the kernel.
2. `clipse board plan board.yaml` previews the reconciliation; `clipse board
   apply board.yaml` executes it.

Re-runs are **idempotent**: each created issue carries a hidden
`<!-- clipse-ref: <ref> sha:<hash> -->` marker in its Linear description, so a
re-apply creates only new issues, updates changed ones (sha differs), skips
unchanged ones, and never touches issues absent from the spec (reported as
orphans, never deleted). `board.yaml` `ref`s are the stable, tracker-agnostic
identity clipse reconciles on. Follow-up: migrate `scripts/smoke/smoke.sh`'s
`seed()` onto `clipse board apply` (retiring the `linear`-CLI stdout scraping)
once the tool has a live-Linear run behind it.

## Kernel invariants (do not violate)

- **`board_status='running'` is entered ONLY via the CAS claim** (`store.ClaimReady`, a `BEGIN IMMEDIATE` compare-and-swap on `board_status='ready' AND claim_lock IS NULL`). Never write `running` directly.
- **`cancelled` is a terminal board status, not a `contract.Column` value** (`issues.board_status` is unconstrained `TEXT`, no `CHECK` constraint — `internal/store/migrations.go`). It is written only by `adoptLinearMove` observing a human-cancelled Linear issue (state `type=="canceled"`, `internal/linear/status.go`'s `statusFromWorkflowName`), is never claimed, and is treated as terminal by `board.Promote`'s dependency gating and by `RecoverOrphans` — same as `done`: a leftover run row on a cancelled issue is restart debris, closed out rather than requeued or blocked.
- **SQLite is runtime truth; Linear is task intent.** The dispatcher is the only *automated* writer of board state. A re-poll never clobbers a dispatcher-owned `board_status` (`UpsertIssue` preserves it on conflict); humans may move cards, and the poll adopts the move only when the issue holds no active claim **and** the observed Linear status isn't `running` (else SQLite wins and the outbox re-asserts — `running` is entered ONLY via the CAS claim, so an unclaimed `running` observed from Linear can never be genuine).
- **Linear is written ONLY through the outbox.** Transitions enqueue a `linear_writes` row in the *same* transaction as the state change (`store.Transition`); `dispatcher.drainOutbox` mirrors pending rows each tick and retries on failure. An outage never loses or duplicates a transition. `clipse status` flags issues with pending (unmirrored) writes.
- **Linear state ownership is configurable, never mixed.** With no `state_label_prefix`, the existing workflow-state mapping is unchanged. With a prefix (for example `clipse:`), startup requires the eight `<prefix>{todo,ready,running,review,merging,done,rework,blocked}` team labels and all non-terminal state reads/writes use those labels without moving the Linear workflow. No state label means `todo`; ambiguous/unknown prefixed labels fail safe to `blocked`; Linear `completed`/`canceled` types remain terminal overrides. Every observed `done` converges through an idempotent outbox write that removes the `agent:<lane>` opt-in label so terminal work leaves the candidate query. To requeue, remove/replace `<prefix>done`, reopen a completed/canceled workflow to any non-terminal state when needed, then restore `agent:<lane>` last.
- **Transient failures auto-recover (bounded); everything else parks in `blocked`.** A transient failure — a worker `block_kind=transient`, a run-level crash / malformed-result / timeout, or a spawn/workspace failure — is deterministically re-queued to its release column (`store.ReleaseTargetColumn`) under a bounded budget: `recover_attempts < recover_cap`, each retry delayed by `recover_backoff_s` via `issues.blocked_until` (a card inside its backoff window is invisible to every claim/peek — the anti-hot-loop guard). `recover_attempts` resets to 0 when the card next advances normally. Once the budget is spent — or for a non-transient block (`capability`/`needs_input`), a `rework_cap` exhaustion, an illegal transition, or an orphan past `max_attempts` (git-operator lane exempt — its bound is `recover_attempts`, not `max_attempts`; see `dispatcher.recoverOrphanRun`) — the card parks in `blocked` with a reason comment and is never auto-requeued (a human must move it). `dispatcher.parkOrRetry` is the single retry-vs-park decision point; the recovery re-queue goes through `store.Transition` (outbox-mirrored) like any other transition. Continuation is bounded by `turn_cap`; a dispatcher-restart orphan is requeued under `max_attempts` (then blocked), and its process is killed with a PID-identity guard *before* any stale-claim release.
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
- **Per-ticket transcript logging**: every coder/coder_docs/reviewer DAC turn
  appends one JSON-object-per-line to `<board_dir>/logs/<ISSUE>.transcript.jsonl`
  (one file per issue, accumulating across every turn/lane/rework it ever
  runs) via `clipse_agent.transcript.TranscriptWriter`, tapped inside
  `dac.drive_turn`'s stream loop through an optional `event_sink`. Event
  types: `turn_start`/`turn_end` (lane, run_id, thread_id, assistant_id,
  model, plus task_text/outcome+tokens respectively), `assistant` (text),
  `tool_call` (name, args), `tool_result` (name, status, content truncated to
  8k chars), `interrupt` (payload repr). A turn that crashes mid-stream still
  emits `{"event": "turn_end", "error": "<exc>"}` after flushing the partial
  message; the 8k truncation caps both `tool_result` content and `assistant`
  text. A write failure is logged to stderr
  and swallowed -- the transcript is a debug aid, never load-bearing for a
  run's outcome. Threaded end to end exactly like the checkpoint DB path:
  `dispatcher.transcriptPath` (derived from `cfg.BoardDir`) ->
  `WorkerSpec.TranscriptPath` -> `--transcript=` argv -> the worker's
  `TranscriptWriter`; disabled (`None`) whenever the flag is omitted, which
  is every hand-built test config that has no `BoardDir` to root a path
  under. The coder graph's clean path always runs a best-effort docs
  sub-turn after the code turn (`run_docs`, lane `coder_docs`), so a single
  successful coder run's transcript legitimately carries two
  `turn_start`/`turn_end` pairs, one per lane.
- **`create_model` routing**: a bare `openai_codex:*` spec string can't go straight into `create_cli_agent` — it routes through LangChain's `init_chat_model`, which raises `ValueError` on the DAC-only `openai_codex` provider (never registered with LangChain). `dac.build_coder_agent` pre-builds a `BaseChatModel` via `deepagents_code.config.create_model(...)` — passing the object instead of the spec string — whenever the provider is `openai_codex`, the profile carries `model_params` (only `create_model` accepts `extra_kwargs`), or the profile carries `context_window_tokens` (see below, needed so the model has a `.profile` to tune); a lane with none of those keeps the plain-string path untouched.
- **`shell_allow_list`**: per-lane shell posture, keyed the same way as `models:`/`model_params:` (`coder` / `coder_docs` / `reviewer`), configured in `clipse.yaml`'s `shell_allow_list:` block. Each key is either the sentinel `all` (**default when omitted**) or an explicit list of allowed command names. `all` threads through as `shell_allow_list=None` on the Python profile, which `dac.build_coder_agent` maps to DAC's unrestricted mode — `auto_approve=True`, no allow-list, no dangerous-pattern checks (redirects/env-prefixes/heredocs). An explicit list threads through as a tuple, mapping to the original restrictive mode — `auto_approve=False, interrupt_shell_only=True, shell_allow_list=[...]` — which also rejects some legitimate command idioms (e.g. `2>&1`) regardless of the list; a reflex live-fire hit ~55 such false rejections across 25 issues, motivating `all` as the default (decision 2026-07-07). `internal/config` validates shape only (`all` or a non-empty list of non-empty strings) — no command vocabulary, keeping the kernel LLM-free; the Go side omits `--shell-allow-list`/`--docs-shell-allow-list` entirely for an `all`-policy lane, and the worker's own default (`None`, unrestricted) lines up so an absent flag is never ambiguous.
- **DAC auto-compaction is always on**: `create_deep_agent` unconditionally installs a `SummarizationMiddleware` that compacts a round's context once it crosses `0.85 x model.profile["max_input_tokens"]` — this is DAC's own behavior, not something clipse opted into. `dac.build_coder_agent` tunes *when* it fires by setting `model.profile["max_input_tokens"]` from the coder/reviewer profile's `context_window_tokens` field (default `200_000`, so compaction triggers around 170k/round) — well under most models' real context window, so a long turn compacts itself instead of ballooning.
- **`max_tokens_per_run` is a per-round context guard, not a cumulative-spend cap**: it bounds the largest single DAC round's input (context) tokens — a post-compaction runaway guard, tripped only if a round still exceeds the ceiling after auto-compaction should have kept it small. `max_runtime_s` and `turn_cap` are the backstops on total work across a run (wall-clock, continuation count); `max_tokens_per_run` bounds one round, not the whole run. See `internal/config.defaultMaxTokensPerRun`'s doc comment for the full rationale.

## Open follow-ups (Phase 2–3)

- **Documentation is a coder-graph step, not a lane** (reversed design decision D3): `graphs/coder.py`'s `run_docs` node drives a best-effort docs DAC turn (restricted `get_coder_docs_profile`) right before the PR is opened, so docs ride the same commit/PR as the code and get reviewed. A merged PR goes `merging → done` directly. This removed the `documentation` column, the `scribe` lane, and the `-docs` branch/worktree apparatus (`EnsureDocs`/`EnsureDocsWorktree`) — and with it the non-fast-forward push bug and the `-docs` worktree leak.
- **Cross-lane claiming**: `board.Next` resolves reviewer outcomes while the issue sits at `review` and git-operator outcomes at `merging`; `store.ClaimColumn` claims per-column (`ClaimReady` still handles only `ready`→`running` for the coder). Shipped (decision P).
- **Workspace cleanup**: `internal/gitops` removes the local coder worktree and branch after a successful merge (decision F). Terminal `done` and `cancelled` transitions now schedule provider-neutral workspace cleanup; Daytona deletion is retried from SQLite, and local cancellation removes the worktree through the same reconciliation path.
- **Rework feedback carries the reviewer's rollup, not its inline findings**: `store.LatestReworkFeedback` → `CLIPSE_REVIEW_FEEDBACK` threads the newest `changes_requested` run's `summary` into the coder's rework re-run (fixed the byte-identical dead-loop). But that summary can go vague ("diff unchanged, same findings") instead of restating the actionable per-line findings, so convergence can hinge on the coder recovering specifics from the PR/context. Thread the reviewer's inline PR comments (or make `graphs/reviewer.py` always restate findings in its summary) so a rework turn always gets the actual asks.
- **Linear `SetState`** — ✅ done (Phase 2): the HTTP client resolves `contract.Column` → per-team workflow-state id (fetch the team's states, map by name via the inverse of `statusFromWorkflowName`, cached) and scopes the candidate-issue query to the configured team.
- **Git-operator is deterministic Go** (`internal/gitops`), not a DAC lane (decision O / amended J) — merge/tag/cleanup + CI-and-branch-protection gating live in the kernel in Phase 3.
- **Worker interrupt-resume (HITL answer)**: the coder graph + `dac.py` support `Command(resume=...)` (unit-tested), but no live producer feeds a human's answer back — a `blocked(needs_input)` issue that is requeued is re-run as a fresh `task_text` turn on the same thread, not resumed with an answer to the pending interrupt. Design the human-answer → resume channel (e.g. a `--resume-payload` sourced from a Linear comment) in Phase 2/3. (The turn-cap `continue` path is unused: the DAC agent runs to completion within one worker turn, emitting `needs_review`/`blocked`.)
- **openai_codex OAuth-token reach (accepted risk)**: `~/.deepagents/.state/chatgpt-auth.json` sits under the `HOME` the coder worker already inherits, reachable by the coder lane's own shell tools — a crafted Linear issue body could steer the agent into reading or exfiltrating it. Unlike the env-var-only `ANTHROPIC_API_KEY`/scoped `gh` token beside it, this is a persistent, account-level credential — a broader blast radius. Accepted for this project's single-tenant, personal-use posture (see `docs/design/2026-07-03-configurable-per-lane-models.md`'s Risks); future hardening: keep the token outside the coder's readable path. With the `shell_allow_list` default now `all` (unrestricted, decision 2026-07-07), the coder's shell has no allow-list gate at all, so a crafted issue body can execute arbitrary commands in the worker's process, not just the handful (`cat`/`find`/`grep`/`rg`) the old restrictive list permitted — widening this risk's reach to any shell-accessible credential or side effect, not just this one file. The C11 injection eval (`evals/test_coder_evals.py::test_c11_injection_canary_stays_out_of_output`) is the standing monitor for this posture; a lane operator who wants the narrower blast radius back can set `shell_allow_list: { coder: [...] }` in `clipse.yaml` to restore the restrictive mode.
- **Coder base-sync + merge-conflict resolution**: `graphs/coder.py` gains a `sync_base` node (`ensure_worktree → sync_base → run_DAC`) that merges `origin/<base_branch>` into the worktree at the start of every coder turn — keeping dependents from building on a stale local base, and surfacing a real conflict as markers in the worktree instead of leaving `internal/gitops.OutcomeStaleBaseConflict`'s `rework` route the dead end it used to be (the coder blindly re-ran its issue task on the same never-merged worktree, re-conflicted, and exhausted `rework_cap` into `blocked`). A conflict is now resolved by the coder itself: the graph runs the git (`sync_base`'s `git merge --no-edit origin/<base>` — merge, never rebase, so `push` stays fast-forward with no force — and `make_commit`'s completing `git commit --no-edit`), while the DAC agent only edits the conflicted files to remove the markers and never runs git for it. Two guards make this safe across turns: a `MERGE_HEAD` check in `sync_base` (before fetch/merge) lets an interrupted resolution turn resume from the still-unresolved index rather than being clobbered by a second merge attempt; an unresolved-marker check in `make_commit` (`grep -lE`, scoped to the coder's own worktree) blocks the commit rather than pushing `<<<<<<<` markers to the branch.
- **`base_branch` reaches the worker via `--base-branch`**: `WorkerSpec.BaseBranch` (from `cfg.Repo.BaseBranch`) is set for every lane, though only the coder's `sync_base` reads it today — harmless unused input for reviewer/git-operator. This also fixed a latent bug where the reviewer's PR-diff base and the coder's PR base both always fell back to `"main"`, since nothing previously threaded the configured value through to either.
