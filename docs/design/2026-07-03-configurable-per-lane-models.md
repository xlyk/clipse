# Configurable per-lane model providers (incl. openai_codex)

**Status:** Proposed · **Date:** 2026-07-03 · **Owner:** Kyle

## Context & problem

Every DAC profile's model is a hardcoded Python string today: `get_coder_profile`/`get_coder_docs_profile` both pin `"anthropic:claude-sonnet-4-6"` (`agent/src/clipse_agent/profiles/coder.py:93,163`), `get_reviewer_profile` pins `"anthropic:claude-opus-4-6"` (`agent/src/clipse_agent/profiles/reviewer.py:108`). There is no config surface — changing the model means editing Python.

The ask: make each lane's model configurable via `clipse.yaml`, and enable DAC's `openai_codex` provider (ChatGPT sign-in, no API key) for the coder lane, targeting `gpt-5.5`.

**The blocker.** `create_cli_agent(model: str | BaseChatModel, ...)` (`deepagents_code/agent.py:1179`) forwards a string straight to `create_deep_agent` → `deepagents._models.resolve_model` (`deepagents/_models.py:23-44`), which for anything that isn't already a `BaseChatModel` calls LangChain's `init_chat_model(model, ...)` at line 44. Verified directly against the repo's `agent/.venv` (deepagents_code 0.1.22, deepagents 0.6.11): calling either `resolve_model("openai_codex:gpt-5.5")` or `init_chat_model("openai_codex:gpt-5.5")` raises `ValueError: Unable to infer model provider for model='openai_codex:gpt-5.5'` immediately — it does not silently misroute to a wrong provider. The mechanism is specific: LangChain's `_parse_model` (`langchain/chat_models/base.py:597-625`) only splits the `provider:model` colon when the prefix is already a key in its fixed `_BUILTIN_PROVIDERS` dict; `"openai_codex"` is a DAC-only provider name never registered with LangChain, so the split never fires, and the fallback prefix-based inference (`_attempt_infer_model_provider`) also matches nothing — hence a hard `ValueError`, not a wrong-provider pick. DAC's own OAuth-aware resolution lives only in `deepagents_code.config.create_model` (`config.py:3545`), which the string path never touches. `resolve_model` *does* pass a `BaseChatModel` instance straight through unchanged (`_models.py:41-42`) — the fix is to pre-build the model object for `openai_codex` and hand `create_cli_agent` the object instead of the spec string.

**The auth model.** `openai_codex` authenticates via a file-backed OAuth token store, not an env var: `default_store_path()` resolves to `~/.deepagents/.state/chatgpt-auth.json` (`deepagents_code/integrations/openai_codex.py:123-133`; `DEFAULT_CONFIG_DIR = Path.home() / ".deepagents"`, `model_config.py:493,499`). A full read of `openai_codex.py` (552 lines) found zero `os.environ` credential reads. The token auto-refreshes (`build_chat_model`'s eager `get_token()`, `openai_codex.py:538`); an absent or dead-refresh token raises `FileNotFoundError`/`CodexAuthExpiredError` (`openai_codex.py:218-225`), both re-raised by `create_model` as `MissingCredentialsError` pointing at `/auth` (`config.py:3742-3756`). `HOME` is already in Go's `defaultEnvAllowlist` (`internal/config/config.go:69-76`), so the worker subprocess sees the same `HOME` as the dispatcher with zero allowlist change — sign-in must simply happen as the same OS user.

## Goals / Non-goals

**Goals**
- Each lane's model is set in `clipse.yaml`, independently: `coder`, `coder_docs` (the coder graph's docs sub-step), `reviewer`.
- `openai_codex:gpt-5.5` works for the coder lane (and any lane, since the fix is provider-driven).
- Zero behavior change for an unconfigured deploy — same hardcoded anthropic defaults as today.
- The Go kernel stays LLM-free: it threads opaque `"provider:model"` strings, never a model catalog.

**Non-goals**
- No `git_operator` model — that lane is deterministic Go (`internal/gitops`), never a DAC agent (decision O/J in the main design doc).
- No change to `create_cli_agent`'s SAFETY block (`auto_approve=False`, `interrupt_shell_only=True`, `shell_allow_list`, `enable_ask_user=True`).
- No reclassification of auth failures in the recovery state machine (flagged under Risks, not fixed here).
- No non-codex provider gets special-cased — the pre-build branch is `openai_codex`-only.

## Decisions

| # | Decision | Rationale |
|---|---|---|
| M1 | Codex model pinned to `gpt-5.5` | User-locked; confirmed valid in `CODEX_MODELS` (`deepagents_code/model_config.py:571-587`: `gpt-5.2`, `gpt-5.3-codex`, `gpt-5.4`, `gpt-5.4-mini`, `gpt-5.5`). |
| M2 | Config-driven (`clipse.yaml`'s `models:` block), not hardcoded | The whole point of the ask; also keeps the Go kernel LLM-free — it validates shape only, never a provider/model vocabulary. |
| M3 | Each lane independently configurable — keyed by *profile identity* (`coder`, `coder_docs`, `reviewer`), not `per_lane` | `per_lane` is concurrency caps, a different axis; `coder_docs` isn't a lane at all (it's a step inside the coder graph per AGENTS.md's reversed decision D3), so it needs its own key sitting alongside, not nested under, `coder`. |
| M4 | DAC pre-build is `openai_codex`-only; every other provider keeps the plain-string path | Minimal blast radius — the proven anthropic path (`profile.model` straight into `create_cli_agent`) is untouched, byte-for-byte. |
| M5 | Unconfigured/omitted keys default to the current hardcoded strings (`anthropic:claude-sonnet-4-6` for coder/coder_docs, `anthropic:claude-opus-4-6` for reviewer) | Backward compatibility: an existing deploy with no `models:` key behaves identically to today. |
| M6 | No `git_operator` entry in `models:` | That lane has no model — it's deterministic Go, never a DAC agent (existing decision O/J). |

## Design

### Config schema

New `Models`/`rawModels` types, added alongside `PerLaneCaps` (`internal/config/config.go:86-90`), following the file's one existing idiom exactly (`Config` concrete field + `rawConfig` pointer mirror + `stringOrDefault` defaulting + a `validate` check appended last, matching how `LaneLabelPrefix`/`CheckpointsDir`/`BoardDir` already work, `config.go:216`, `:286-291`):

```go
type Models struct {
	Coder     string `yaml:"coder"`
	CoderDocs string `yaml:"coder_docs"`
	Reviewer  string `yaml:"reviewer"`
}

type rawModels struct {
	Coder     *string `yaml:"coder"`
	CoderDocs *string `yaml:"coder_docs"`
	Reviewer  *string `yaml:"reviewer"`
}
```

New defaults next to the other `default*` consts (`config.go:11-62`): `defaultModelCoder = "anthropic:claude-sonnet-4-6"`, `defaultModelCoderDocs = "anthropic:claude-sonnet-4-6"`, `defaultModelReviewer = "anthropic:claude-opus-4-6"`. `Config` gains `Models Models `yaml:"models"`` after `Worker` (`config.go:153`); `rawConfig` mirrors it (`config.go:211`); `Load`'s struct literal (`config.go:245-270`) fills it via `stringOrDefault` exactly like every other optional string field.

`validate` (`config.go:304-396`) appends, last (same placement rationale as the existing `worker.command` checks, `config.go:379-386` — so it never shadows an earlier single-field fixture), a new `validateModelSpec(field, spec string) error` using `strings.Cut(spec, ":")` (new `strings` import) to require a non-empty provider and a non-empty model half — shape only, never vocabulary, keeping the kernel LLM-free. Because `Load`'s defaulting always runs first, `cfg.Models.*` is never actually empty by the time `validate` sees it; the empty-string branch is a defensive backstop, not a reachable path today.

`configs/clipse.example.yaml` gets a new `models:` block placed after the `worker:` section, before `checkpoints_dir:` (`configs/clipse.example.yaml:91`):

```yaml
models:
  coder: "openai_codex:gpt-5.5"
  coder_docs: "openai_codex:gpt-5.5"
  reviewer: "anthropic:claude-opus-4-6"
```

**Tests** (`internal/config/config_test.go`): extend `TestLoad_ValidFullConfig` (24-136) and `TestLoad_MinimalConfigGetsDefaults` (138-207) with `Models.*` assertions; add a new `TestLoad_PartialModelsOverride` that sets only `models.coder` and asserts `CoderDocs`/`Reviewer` still resolve to their defaults independently — proving M3's per-key defaulting rather than assuming it from `stringOrDefault` mechanics; append three cases to `TestLoad_InvalidConfigs`'s table (261-524) — missing colon, empty provider, empty model, each isolating the failure the same way the existing `worker.command` cases do (445-485). `TestLoad_ExampleConfigLoadsWithoutError` (541-549) needs no edit — it exercises the new block automatically.

### Go threading

`WorkerSpec` (`internal/spawn/spawn.go:44-67`) gains two fields, following the exact "optional — flag appended only when non-empty" convention already used for `CheckpointDB`/`MaxTokens`:

```go
Model     string // e.g. "anthropic:claude-sonnet-4-6" or "openai_codex:gpt-5.5"
DocsModel string // set only for the coder lane's docs sub-step
```

`workerArgs` (`internal/spawn/local.go:46-61`) appends `--model=<value>` / `--docs-model=<value>` after the existing `--max-tokens` conditional — pure, unit-tested helper, no subprocess involved.

**Resolution happens in exactly one seam.** `spawnAttempt` (`dispatcher/spawn.go:30-67`) is the *only* place a `WorkerSpec` is constructed, and it already receives `lane` as a parameter. Both its callers already funnel through it: `spawnClaim` (`dispatcher/schedule.go:222`, covering both the fresh-claim coder pool and the review-column reviewer claim) and `applyContinue` (`dispatcher/reconcile.go:252`, the turn-cap continuation path). A new unexported helper resolves lane → model spec(s):

```go
func (d *Dispatcher) modelsFor(lane string) (model, docsModel string) {
	switch lane {
	case string(contract.LaneCoder):
		return d.cfg.Models.Coder, d.cfg.Models.CoderDocs
	case string(contract.LaneReviewer):
		return d.cfg.Models.Reviewer, ""
	default:
		return "", ""
	}
}
```

`spawnAttempt` itself changes to call it: add `model, docsModel := d.modelsFor(lane)` immediately before the `spec := spawn.WorkerSpec{...}` literal, and add `Model: model, DocsModel: docsModel,` fields to that literal alongside the existing `CheckpointDB`/`MaxTokens` assignments — this is the actual wiring; `modelsFor` alone does nothing until `spawnAttempt` calls it.

Git-operator never reaches `spawnAttempt`: `claimAndRunGitops` (`dispatcher/gitops.go:45-70`) → `runGitopsClaim` (`gitops.go:81-104`) calls `d.gitOps(ctx, spec)` — a `gitops.Spec` with no model-related field (`gitops.go:90-95`) — directly. It's a third, disjoint path; `modelsFor`'s `default` branch is defensive only.

**Tests**: `internal/spawn/argv_test.go`'s `TestWorkerArgs` table gets new cases (model-only, docs-model-only, both-empty, coder-shape, reviewer-shape-no-docs-model). A new `dispatcher/spawn_test.go` adds `TestDispatcher_ModelsFor`, table-driven over lane, mirroring the hand-built-`WorkerSpec` pattern already used in `dispatcher/recover_test.go:26`. That unit test alone doesn't prove the wiring, so a new `dispatcher/models_test.go` adds an integration-level test mirroring `dispatcher/checkpoint_test.go`'s `TestSpawnAttempt_WiresCheckpointDBAndMaxTokens`: tick a real `Dispatcher` with `cfg.Models` set and assert `fakeSpawner.Specs()[0].Model`/`.DocsModel` via a coder claim, assert again via a reviewer claim (`claimSimpleColumn`) proving `DocsModel` stays empty there, and assert once more via `applyContinue`'s re-spawn path proving the same resolution applies on continuation.

### Python worker & profiles

`_build_parser` (`agent/src/clipse_agent/worker.py:46-55`) gains `--model` and `--docs-model`, both defaulting to `""` (mirrors the existing `--checkpoint-db` empty-means-unset convention).

Profile factories gain an optional override, defaulting to today's literal:

```python
def get_coder_profile(model: str | None = None) -> CoderProfile:
    return CoderProfile(..., model=model if model is not None else _DEFAULT_MODEL, ...)
```

(same for `get_coder_docs_profile` in `profiles/coder.py:153`, and `get_reviewer_profile` in `profiles/reviewer.py:95`). `model if model is not None else default` — not `model or default` — matches the existing idiom in `build_coder_graph`/`build_reviewer_graph` (`graphs/coder.py:717-718`) and correctly distinguishes "omitted" from "explicit falsy".

`_run_lane_graph` (`worker.py:105-154`) gains an `extra_kwargs: dict[str, Any] | None = None` parameter forwarded into every `build_graph(checkpointer=..., **(extra_kwargs or {}))` call — the `or {}` guard matters because both existing call sites currently pass no `extra_kwargs`, and `**None` raises `TypeError`. `_dispatch` (`worker.py:157-182`) builds the profile(s) up front and passes them in:

```python
if lane == Lane.coder:
    profile = get_coder_profile(args.model or None)
    docs_profile = get_coder_docs_profile(args.docs_model or None)
    return await _run_lane_graph(args, build_coder_graph, lane=Lane.coder,
                                  extra_kwargs={"profile": profile, "docs_profile": docs_profile})
if lane == Lane.reviewer:
    profile = get_reviewer_profile(args.model or None)
    return await _run_lane_graph(args, build_reviewer_graph, lane=Lane.reviewer,
                                  extra_kwargs={"profile": profile})
```

No graph-internal change needed: `build_coder_graph` (`graphs/coder.py:684-741`) and `build_reviewer_graph` (`graphs/reviewer.py:562-588`) already accept injected `profile=`/`docs_profile=`, falling back to `get_coder_profile()`/`get_coder_docs_profile()`/`get_reviewer_profile()` when `None` (`coder.py:717-718`, `reviewer.py:588`). One `--model` flag is reused for whichever lane the worker process is dispatched to (one lane per process); `--docs-model` only affects the coder lane's docs sub-step; the reviewer's `extra_kwargs` deliberately omits `docs_profile` (`build_reviewer_graph` has no such parameter).

**Required, not just additive, test change**: `agent/tests/test_worker.py`'s `_fake_build_coder_graph` (60-73) currently has `factory(*, checkpointer=None)` — once `_dispatch` passes `profile=`/`docs_profile=`, every existing happy-path test in that file will `TypeError`. Fix: `factory(*, checkpointer=None, **kwargs)`, threading `kwargs` into an optional `kwarg_calls` list new tests can assert against; `_run_main_capture` (93-111) grows a passthrough `kwarg_calls` parameter. New tests: model threaded into coder profile, docs-model threaded independently, both-omitted defaults preserved, model threaded into reviewer profile (no `docs_profile` key).

### DAC model-resolution fix

`dac.build_coder_agent` (`agent/src/clipse_agent/dac.py:82-121`) is the single seam both lanes share as `agent_factory`. The fix is provider-driven, not lane-driven — so a reviewer profile pointed at `openai_codex:*` gets the same treatment automatically:

```python
provider = profile.model.split(":", 1)[0]
try:
    model: str | Any = profile.model
    if provider == CODEX_PROVIDER:
        model = create_model(profile.model).model
    return create_cli_agent(model, profile.assistant_id, ...)  # SAFETY block unchanged
except Exception as exc:
    raise DacError(f"failed to build DAC agent for assistant_id={profile.assistant_id!r}: {exc}") from exc
```

New imports: `from deepagents_code.config import create_model`, `from deepagents_code.model_config import CODEX_PROVIDER`. `ModelResult` is a frozen dataclass whose `.model` field is the `BaseChatModel` to pass (`deepagents_code/config.py:3465-3485`); `create_model`'s signature and codex branch are at `config.py:3545-3591` and `:3653-3659`. The existing `try/except Exception` already wraps both `create_model` and `create_cli_agent` into `DacError` — a `MissingCredentialsError` (unsigned-in ChatGPT) needs no new except branch. The three existing `build_coder_agent` tests (safe-shell-enforcement, forwards-checkpointer, wraps-create_cli_agent-errors) all use the anthropic profile and take the unchanged string path, confirming backward compatibility.

**New tests** (`agent/tests/test_dac.py`): codex provider pre-builds and passes the model *object* (never the string) to `create_cli_agent`; a non-codex (anthropic) provider passes the string unchanged and never calls `create_model`; a `create_model` failure is wrapped as `DacError` with the original exception as `__cause__`.

Note: `profile.model.split(":", 1)[0]` assumes `provider:model` shape always holds — true today for every profile, and now enforced upstream by Go's `validateModelSpec`. A bare model name with no colon would silently take the (broken-for-codex) string path rather than erroring — a safe failure direction (falls toward the proven path), not a blocker, but only reachable if something bypasses config-driven construction entirely.

## Data flow

```
clipse.yaml (models: coder/coder_docs/reviewer)
  → internal/config.Load          — stringOrDefault fills omitted keys; validateModelSpec checks "provider:model" shape
  → config.Config.Models{Coder, CoderDocs, Reviewer}
  → dispatcher.spawnAttempt → d.modelsFor(lane)
  → spawn.WorkerSpec{Model, DocsModel}
  → spawn.workerArgs()            — appends "--model=<spec>" / "--docs-model=<spec>" to argv
  → clipse-worker argparse        — args.model / args.docs_model ("" when unset)
  → clipse_agent.worker._dispatch — get_coder_profile(args.model or None) / get_coder_docs_profile(...) / get_reviewer_profile(...)
  → CoderProfile.model / ReviewerProfile.model  (plain "provider:model" str)
  → build_coder_graph(profile=, docs_profile=) / build_reviewer_graph(profile=)
  → dac.build_coder_agent(profile, checkpointer, cwd)
  → provider == "openai_codex"?  create_model(profile.model).model  :  profile.model (string, unchanged)
  → deepagents_code.create_cli_agent(model, ...) → deepagents.resolve_model  — passthrough for a BaseChatModel object, init_chat_model for a string
  → live DAC agent
```

## Backward compatibility & defaults

Two independent, redundant defaulting layers land on the same literal strings, by design:

1. **Go**: an omitted `models:` key defaults to `anthropic:claude-sonnet-4-6` (coder/coder_docs) or `anthropic:claude-opus-4-6` (reviewer) at `Load` time — so a dispatcher-spawned worker's `--model`/`--docs-model` is *always* non-empty, matching today's hardcoded values exactly.
2. **Python**: `get_coder_profile(model=None)` / `get_coder_docs_profile(model=None)` / `get_reviewer_profile(model=None)` fall back to the same literals — this covers a bare `clipse-worker` invocation outside the dispatcher (tests, manual runs) where `--model` is never passed.

Net effect: an unconfigured `clipse.yaml` is unaffected, and the anthropic path in `dac.build_coder_agent` is untouched (string straight through to `create_cli_agent`, identical to pre-change behavior). No `internal/config` env-allowlist change, no `CoderProfile`/`ReviewerProfile` dataclass field-type change (`model: str` stays `str` in both — coder.py:79, reviewer.py:90).

## Auth prerequisite & rollout

`openai_codex` requires a **one-time, interactive, per-dispatcher-host** ChatGPT sign-in, run as the exact OS user the dispatcher process runs as (so `Path.home()` inside the worker subprocess — which inherits `HOME` via the existing `defaultEnvAllowlist`, `internal/config/config.go:69-76` — resolves to the same `~/.deepagents/.state/chatgpt-auth.json` the sign-in wrote):

```
uv --project agent run dcode
# inside the TUI: /auth -> select openai_codex -> complete the browser sign-in
```

`dcode` is a real installed console script (`agent/.venv/bin/dcode`, `deepagents_code-0.1.22.dist-info/entry_points.txt`). This requires no new dependency: `/auth`'s codex option is gated on `"openai" in installed` (`deepagents_code/auth_commands.py:264-267`), and `langchain-openai` is already a direct dependency of `deepagents-code==0.1.22` per `agent/uv.lock`, installed at `agent/.venv/lib/python3.13/site-packages/langchain_openai-1.3.3.dist-info` — confirmed by running `get_available_models()` in the venv and seeing `openai_codex` already present. No `pyproject.toml` change needed.

For a headless host with no browser, use the manual-URL fallback (`openai_codex.run_browser_login(open_browser=False)`, `openai_codex.py:349-360`) or SSH-forward the local OAuth callback port (`openai_codex.py:325-338`).

The token auto-refreshes thereafter (`build_chat_model`'s eager `get_token()`, `openai_codex.py:538`) — no ongoing action needed unless the refresh token itself expires/is revoked.

**Failure surfacing (traced end to end):** `MissingCredentialsError` (not-signed-in or refresh-expired) → caught by `build_coder_agent`'s existing `except Exception` → wrapped `DacError` (`dac.py:104-121`) → uncaught through the coder graph's node → `worker.py`'s top-level `except Exception` (`worker.py:185-202`, "a worker must never die with no result") → `_blocked_transient` (`worker.py:84-102`) → `outcome=blocked, block_kind=transient`. This is exactly AGENTS.md's existing transient-failure invariant: bounded auto-retry under `recover_cap` (default 5), delayed by `recover_backoff_s`, then parks in `blocked` with a reason comment — no code change required for this to route correctly. Because a stale credential can't self-heal, expect the card to exhaust its retry budget quickly and park; the fix is the human sign-in step above, then moving the card back to `ready`.

## Testing strategy

TDD throughout — failing test first, per area, `make test` (`test-go` + `test-py`) is the gate:

- **Go config**: `internal/config/config_test.go` — extend `TestLoad_ValidFullConfig`, `TestLoad_MinimalConfigGetsDefaults`, add `TestLoad_PartialModelsOverride` (single-key override proves independent defaulting), add 3 cases to `TestLoad_InvalidConfigs`.
- **Go spawn/dispatch**: `internal/spawn/argv_test.go`'s `TestWorkerArgs` (new cases); new `dispatcher/spawn_test.go` with `TestDispatcher_ModelsFor` (table-driven over lane); new `dispatcher/models_test.go` with an end-to-end `Dispatcher.Tick` assertion on `fakeSpawner.Specs()[0].Model`/`.DocsModel` across a coder claim, a reviewer claim, and an `applyContinue` re-spawn, mirroring `dispatcher/checkpoint_test.go`'s `TestSpawnAttempt_WiresCheckpointDBAndMaxTokens`.
- **Python profiles**: `agent/tests/test_profiles.py`, `test_coder_docs_profile.py`, `test_reviewer_profile.py` — override + default-preserved pairs.
- **Python worker CLI**: `agent/tests/test_worker.py` — required `**kwargs` fix to `_fake_build_coder_graph`/`_run_main_capture` (existing suite would otherwise `TypeError` once `_dispatch` starts passing `profile=`/`docs_profile=`), plus new model-threading tests for both lanes.
- **Python DAC resolution**: `agent/tests/test_dac.py` — codex pre-build, anthropic passthrough, `MissingCredentialsError` wrapping.

Baseline captured pre-change: `cd agent && uv run pytest tests/test_profiles.py tests/test_coder_docs_profile.py tests/test_reviewer_profile.py tests/test_worker.py -q` → 45 passed. Keep green throughout.

## Risks & mitigations

- **The ChatGPT OAuth token this rollout places on disk is reachable by the coder lane's own shell tools.** `~/.deepagents/.state/chatgpt-auth.json` sits under the same `HOME` the coder worker inherits via `defaultEnvAllowlist`, and the coder's shell allow-list (`agent/src/clipse_agent/profiles/coder.py`'s `_SHELL_ALLOW_LIST`) already includes `cat`, `find`, `grep`, `rg` — tools a Linear issue body (untrusted input, per this design's own threat model) could steer the agent into using to read and exfiltrate the token, e.g. via a commit, a PR body, or an allowed `gh` call. Unlike the existing named secrets (`ANTHROPIC_API_KEY`, a scoped `gh` token) — both env-var-only and independently revocable/scoped — a ChatGPT OAuth refresh token is a persistent, account-level credential, and this design is what newly places that class of secret somewhere the coder's own allow-listed tools can already read it. Accepted as a residual risk consistent with this project's single-tenant/personal-use posture, but flagged because its blast radius is broader than the secrets it sits alongside; a future hardening pass should consider keeping the token outside the coder worktree's readable path, or scrubbing it from the filesystem the coder's shell can reach before invoking coder tools.
- **`MissingCredentialsError` is not transient but is classified as one.** An unsigned-in/expired ChatGPT credential burns the entire `recover_cap` retry budget in one pass (every retry fails identically) before parking — expected under the current bounded-retry design, but operationally more fragile than a missing `ANTHROPIC_API_KEY` (OAuth tokens expire; API keys don't). No code change in this slice; flagged as a candidate for reclassifying `MissingCredentialsError` specifically as a non-transient `capability` block in a future change.
- **Coder and reviewer can be pointed at the identical model.** `validateModelSpec` checks shape only (`provider:model`), so nothing stops an operator from setting `models.coder` and `models.reviewer` to the same spec — silently defeating the reviewer's documented independence rationale (`profiles/reviewer.py`'s module docstring and its `test_reviewer_model_is_distinct_from_coder_model` test encode that the reviewer should run a different model family so it isn't approving its own sibling's code). No validation added here — keeping the kernel LLM-free means shape-only checks, not a provider/model vocabulary — but worth a comment on `validateModelSpec` or the example config warning operators to keep the two distinct.
- **A bare model name (no `provider:model` colon) would silently take the broken-for-codex string path** rather than erroring, if it ever bypassed config. Mitigated: `validateModelSpec` in `internal/config` already rejects any `clipse.yaml` value without the colon shape, so this is unreachable via the supported config path.
- **Headless dispatcher hosts** can't complete the browser OAuth loopback directly — mitigated via SSH port-forwarding or the manual-URL fallback, both documented in the rollout steps above.
- **Sign-in must run as the same OS user as the dispatcher process** (HOME must match) — documented explicitly; a mismatch produces the identical `MissingCredentialsError` → transient-block symptom, which can look like a fresh auth problem rather than a user mismatch.
- **Two independent default sources** (Go's `default*Model` consts and Python's `_DEFAULT_MODEL` constants) must stay in sync — both currently hardcode the same literals; a future change to one without the other silently diverges. Mitigated by doc comments in `internal/config/config.go` cross-referencing the Python profile defaults they mirror.

## Open questions

- Whether `MissingCredentialsError` should be reclassified as a non-transient `capability` block rather than falling through the generic transient-crash path (see Risks) — deliberately deferred, no decision made.
