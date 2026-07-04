# Configurable per-lane model providers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make each DAC lane's model configurable via `clipse.yaml` and enable the `openai_codex` (ChatGPT sign-in) provider for the coder lane.

**Architecture:** A new `models:` block in `clipse.yaml` (keyed by profile identity: `coder`, `coder_docs`, `reviewer`) flows Go-side through `config.Load` → `dispatcher.spawnAttempt` → `WorkerSpec` → `--model`/`--docs-model` CLI flags → the Python worker's profile factories → `dac.build_coder_agent`. For `openai_codex`, `dac.build_coder_agent` pre-builds a `BaseChatModel` object via DAC's own `create_model` (LangChain's `init_chat_model` cannot resolve the DAC-only `openai_codex` provider and raises `ValueError`); every other provider keeps the plain-string path.

**Tech Stack:** Go 1.25 (stdlib `testing`, `modernc.org/sqlite`, `gopkg.in/yaml.v3`), Python 3.13 + uv (pydantic v2, `deepagents-code==0.1.22`, langgraph), JSON-schema codegen.

**Source spec:** `docs/design/2026-07-03-configurable-per-lane-models.md` (read it — every task below cites it).

## Global Constraints

- Go: check and wrap every error (`fmt.Errorf("...: %w", err)`); no swallowed errors. Runtime logs are `log/slog` JSON only. Table-driven tests, stdlib `testing` only (no testify). Interfaces at the consumption site.
- Python: pydantic v2; `model if model is not None else default` (never `model or default`) for override defaulting; ruff clean.
- The Go kernel stays **LLM-free**: it threads opaque `"provider:model"` strings and validates *shape only* (`provider:model` has both halves) — never a model catalog.
- DAC pre-build is **`openai_codex`-only**; the anthropic string path is untouched byte-for-byte.
- Defaults preserve today's behavior: `anthropic:claude-sonnet-4-6` (coder, coder_docs), `anthropic:claude-opus-4-6` (reviewer). An unconfigured `clipse.yaml` behaves identically to today.
- `create_cli_agent`'s SAFETY block is unchanged: `auto_approve=False, interrupt_shell_only=True, shell_allow_list=[...], enable_ask_user=True`.
- Do not hand-edit generated files (`internal/contract/contract.go`, `agent/src/clipse_agent/contract.py`).
- Commits: Conventional Commits, lowercase, no trailing period, no AI/Claude signature, one concern per commit. `make test` is the gate. Never `git add -A`/`.`.

## File Structure

- `internal/config/config.go` — add `Models`/`rawModels` types, `default*Model` consts, `Config.Models` field + `rawConfig` mirror + `Load` defaulting, `validateModelSpec`.
- `configs/clipse.example.yaml` — new `models:` block.
- `internal/spawn/spawn.go` — `WorkerSpec.Model` / `.DocsModel`.
- `internal/spawn/local.go` — `workerArgs` emits `--model` / `--docs-model`.
- `dispatcher/spawn.go` — `modelsFor(lane)` helper + `WorkerSpec` literal wiring.
- `agent/src/clipse_agent/profiles/coder.py`, `profiles/reviewer.py` — optional `model` param on the three factories.
- `agent/src/clipse_agent/worker.py` — `--model`/`--docs-model` args + `_run_lane_graph(extra_kwargs=...)` + `_dispatch` profile construction.
- `agent/src/clipse_agent/dac.py` — `build_coder_agent` codex pre-build branch.
- `AGENTS.md` — per-lane model config note, codex auth gate, the str-vs-`BaseChatModel` gotcha.

Order: Go config → Go spawn → Go dispatcher → Python profiles/worker → Python DAC → docs. Each task is independently testable.

---

### Task 1: Go config — `models:` block, defaults, validation

**Files:**
- Modify: `internal/config/config.go` (types near `PerLaneCaps` `:86-90`; consts `:11-62`; `Config` `:153`; `rawConfig` `:211`; `Load` literal `:245-270`; `validate` `:304-396`)
- Modify: `configs/clipse.example.yaml` (after `worker:`, before `checkpoints_dir:` `:91`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.Models{Coder, CoderDocs, Reviewer string}`; `cfg.Models` field; consts `defaultModelCoder`, `defaultModelCoderDocs`, `defaultModelReviewer`.

- [ ] **Step 1: Write failing tests.** In `config_test.go`, add `Models` assertions to `TestLoad_ValidFullConfig` and `TestLoad_MinimalConfigGetsDefaults`; add a new test and three invalid cases:

```go
func TestLoad_PartialModelsOverride(t *testing.T) {
	// only models.coder set -> coder_docs & reviewer keep their independent defaults
	cfg := loadYAML(t, baseValidYAML+`
models:
  coder: "openai_codex:gpt-5.5"
`)
	if cfg.Models.Coder != "openai_codex:gpt-5.5" {
		t.Errorf("Coder = %q, want openai_codex:gpt-5.5", cfg.Models.Coder)
	}
	if cfg.Models.CoderDocs != "anthropic:claude-sonnet-4-6" {
		t.Errorf("CoderDocs = %q, want default", cfg.Models.CoderDocs)
	}
	if cfg.Models.Reviewer != "anthropic:claude-opus-4-6" {
		t.Errorf("Reviewer = %q, want default", cfg.Models.Reviewer)
	}
}
```

Append to `TestLoad_InvalidConfigs`'s table: `{name: "model missing colon", yaml: `models:\n  coder: "gpt-5.5"`, wantErr: "models.coder"}`, `{name: "model empty provider", yaml: `models:\n  coder: ":gpt-5.5"`, ...}`, `{name: "model empty name", yaml: `models:\n  coder: "openai_codex:"`, ...}`.

- [ ] **Step 2: Run tests, verify they fail.** Run: `cd /Users/xlyk/Code/clipse && go test ./internal/config/ -run 'TestLoad' -v` — Expected: FAIL (unknown field `Models`).

- [ ] **Step 3: Implement.** Add types (near `:86`):

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

Add consts (near `:11-62`):

```go
const (
	// Keep in sync with the Python profile defaults in
	// agent/src/clipse_agent/profiles/{coder,reviewer}.py — a divergence
	// silently changes an unconfigured deploy's model.
	defaultModelCoder     = "anthropic:claude-sonnet-4-6"
	defaultModelCoderDocs = "anthropic:claude-sonnet-4-6"
	defaultModelReviewer  = "anthropic:claude-opus-4-6"
)
```

Add `Models Models `yaml:"models"`` to `Config` (after `Worker`, `:153`) and `Models *rawModels `yaml:"models"`` to `rawConfig` (`:211`). In `Load`'s literal (`:245-270`):

```go
Models: Models{
	Coder:     stringOrDefault(rawModelField(raw.Models, func(m *rawModels) *string { return m.Coder }), defaultModelCoder),
	CoderDocs: stringOrDefault(rawModelField(raw.Models, func(m *rawModels) *string { return m.CoderDocs }), defaultModelCoderDocs),
	Reviewer:  stringOrDefault(rawModelField(raw.Models, func(m *rawModels) *string { return m.Reviewer }), defaultModelReviewer),
},
```

(If a `rawModelField` nil-safe helper is heavier than the file's idiom, instead default `raw.Models` to `&rawModels{}` when nil, then read `.Coder` etc. directly with `stringOrDefault` — match whichever pattern the surrounding optional fields already use.) Append to `validate` (last, `:~386`):

```go
func validateModelSpec(field, spec string) error {
	provider, name, ok := strings.Cut(spec, ":")
	if !ok || provider == "" || name == "" {
		return fmt.Errorf("%s must be \"provider:model\" with both halves non-empty, got %q", field, spec)
	}
	return nil
}
```

Call it for the three fields in `validate`; add the `strings` import.

- [ ] **Step 4: Update the example config.** In `configs/clipse.example.yaml` after the `worker:` section:

```yaml
# models: the "provider:model" spec per DAC profile. Omit any key to keep the
# built-in default. Keep coder and reviewer on different model families so the
# reviewer isn't rubber-stamping its own sibling's code. openai_codex:* uses a
# one-time ChatGPT sign-in on the dispatcher host (see AGENTS.md), no API key.
models:
  coder: "anthropic:claude-sonnet-4-6"
  coder_docs: "anthropic:claude-sonnet-4-6"
  reviewer: "anthropic:claude-opus-4-6"
```

- [ ] **Step 5: Run tests, verify pass.** Run: `go test ./internal/config/ -v` — Expected: PASS (incl. `TestLoad_ExampleConfigLoadsWithoutError`).

- [ ] **Step 6: Commit.**

```bash
git add internal/config/config.go configs/clipse.example.yaml internal/config/config_test.go
git commit -m "feat(config): add per-lane models config with shape validation"
```

---

### Task 2: Go spawn — `WorkerSpec` fields + `workerArgs` flags

**Files:**
- Modify: `internal/spawn/spawn.go:44-67` (WorkerSpec)
- Modify: `internal/spawn/local.go:46-61` (workerArgs)
- Test: `internal/spawn/argv_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `WorkerSpec.Model string`, `WorkerSpec.DocsModel string`; `workerArgs` emits `--model=<v>` / `--docs-model=<v>` when non-empty.

- [ ] **Step 1: Write failing tests.** Add cases to `TestWorkerArgs`:

```go
{name: "model only", spec: WorkerSpec{Lane: "coder", Model: "openai_codex:gpt-5.5"}, want: []string{..., "--model=openai_codex:gpt-5.5"}},
{name: "docs-model only", spec: WorkerSpec{Lane: "coder", DocsModel: "anthropic:claude-sonnet-4-6"}, want: []string{..., "--docs-model=anthropic:claude-sonnet-4-6"}},
{name: "both empty omits both flags", spec: WorkerSpec{Lane: "reviewer"}, wantAbsent: []string{"--model=", "--docs-model="}},
```

- [ ] **Step 2: Run, verify fail.** Run: `go test ./internal/spawn/ -run TestWorkerArgs -v` — Expected: FAIL (unknown field `Model`).

- [ ] **Step 3: Implement.** Add to `WorkerSpec`:

```go
Model     string // "provider:model" for this lane's DAC agent; empty = worker default
DocsModel string // "provider:model" for the coder docs sub-step; empty = worker default
```

In `workerArgs`, after the `--max-tokens` conditional:

```go
if spec.Model != "" {
	args = append(args, "--model="+spec.Model)
}
if spec.DocsModel != "" {
	args = append(args, "--docs-model="+spec.DocsModel)
}
```

- [ ] **Step 4: Run, verify pass.** Run: `go test ./internal/spawn/ -v` — Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/spawn/spawn.go internal/spawn/local.go internal/spawn/argv_test.go
git commit -m "feat(spawn): thread model/docs-model into worker argv"
```

---

### Task 3: Go dispatcher — `modelsFor` + `spawnAttempt` wiring

**Files:**
- Modify: `dispatcher/spawn.go:30-67` (spawnAttempt + new helper)
- Test: `dispatcher/spawn_test.go` (new), `dispatcher/models_test.go` (new)

**Interfaces:**
- Consumes: `config.Models` (Task 1), `WorkerSpec.Model/DocsModel` (Task 2), `contract.LaneCoder`/`LaneReviewer`.
- Produces: `(d *Dispatcher) modelsFor(lane string) (model, docsModel string)`.

- [ ] **Step 1: Write failing unit test.** `dispatcher/spawn_test.go`:

```go
func TestDispatcher_ModelsFor(t *testing.T) {
	d := &Dispatcher{cfg: config.Config{Models: config.Models{
		Coder: "openai_codex:gpt-5.5", CoderDocs: "anthropic:claude-sonnet-4-6", Reviewer: "anthropic:claude-opus-4-6",
	}}}
	for _, tc := range []struct{ lane, wantModel, wantDocs string }{
		{string(contract.LaneCoder), "openai_codex:gpt-5.5", "anthropic:claude-sonnet-4-6"},
		{string(contract.LaneReviewer), "anthropic:claude-opus-4-6", ""},
		{string(contract.LaneGitOperator), "", ""},
	} {
		m, dm := d.modelsFor(tc.lane)
		if m != tc.wantModel || dm != tc.wantDocs {
			t.Errorf("modelsFor(%q) = (%q,%q), want (%q,%q)", tc.lane, m, dm, tc.wantModel, tc.wantDocs)
		}
	}
}
```

- [ ] **Step 2: Write failing integration test.** `dispatcher/models_test.go`, mirroring `dispatcher/checkpoint_test.go`'s `TestSpawnAttempt_WiresCheckpointDBAndMaxTokens`: build a `Dispatcher` with a `fakeSpawner` and `cfg.Models` set, seed a claimable coder issue, `Tick`, then assert `fakeSpawner.Specs()[0].Model == "openai_codex:gpt-5.5"` and `.DocsModel == "anthropic:claude-sonnet-4-6"`. Add a reviewer-claim sub-case asserting `.DocsModel == ""`, and an `applyContinue` sub-case asserting the same resolution on the continuation re-spawn.

- [ ] **Step 3: Run, verify fail.** Run: `go test ./dispatcher/ -run 'ModelsFor|Models' -v` — Expected: FAIL (`modelsFor` undefined).

- [ ] **Step 4: Implement.** In `dispatcher/spawn.go`:

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

In `spawnAttempt`, immediately before the `spec := spawn.WorkerSpec{...}` literal add `model, docsModel := d.modelsFor(lane)`, and add `Model: model, DocsModel: docsModel,` to the literal alongside `CheckpointDB`/`MaxTokens`.

- [ ] **Step 5: Run, verify pass.** Run: `go test ./dispatcher/ -v && go test -race ./...` — Expected: PASS, race-clean.

- [ ] **Step 6: Commit.**

```bash
git add dispatcher/spawn.go dispatcher/spawn_test.go dispatcher/models_test.go
git commit -m "feat(dispatcher): resolve per-lane model into workerspec"
```

---

### Task 4: Python profiles + worker CLI threading

**Files:**
- Modify: `agent/src/clipse_agent/profiles/coder.py` (`get_coder_profile` `:84`, `get_coder_docs_profile` `:153`)
- Modify: `agent/src/clipse_agent/profiles/reviewer.py` (`get_reviewer_profile` `:95`)
- Modify: `agent/src/clipse_agent/worker.py` (`_build_parser` `:46-55`, `_run_lane_graph` `:105-154`, `_dispatch` `:157-182`)
- Test: `agent/tests/test_profiles.py`, `test_coder_docs_profile.py`, `test_reviewer_profile.py`, `test_worker.py`

**Interfaces:**
- Produces: `get_coder_profile(model: str | None = None)`, `get_coder_docs_profile(model=None)`, `get_reviewer_profile(model=None)`; worker args `--model`, `--docs-model`; `_run_lane_graph(..., extra_kwargs: dict[str, Any] | None = None)`.

- [ ] **Step 1: Write failing profile tests.** e.g. `test_profiles.py`:

```python
def test_get_coder_profile_model_override():
    assert get_coder_profile("openai_codex:gpt-5.5").model == "openai_codex:gpt-5.5"

def test_get_coder_profile_default_preserved():
    assert get_coder_profile().model == "anthropic:claude-sonnet-4-6"
```

(mirror for `get_coder_docs_profile`, `get_reviewer_profile`; keep the existing `test_reviewer_model_is_distinct_from_coder_model`.)

- [ ] **Step 2: Fix the worker-test fake, then add failing threading tests.** In `test_worker.py`, change `_fake_build_coder_graph(*, checkpointer=None)` → `(*, checkpointer=None, **kwargs)` and record `kwargs` into a `kwarg_calls` list; thread a `kwarg_calls` passthrough into `_run_main_capture`. Add:

```python
def test_dispatch_threads_model_into_coder_profile(...):
    calls = run_worker(["--lane=coder", "--model=openai_codex:gpt-5.5", "--docs-model=anthropic:claude-sonnet-4-6", ...])
    assert calls[-1]["profile"].model == "openai_codex:gpt-5.5"
    assert calls[-1]["docs_profile"].model == "anthropic:claude-sonnet-4-6"

def test_dispatch_reviewer_has_no_docs_profile(...):
    calls = run_worker(["--lane=reviewer", "--model=anthropic:claude-opus-4-6", ...])
    assert calls[-1]["profile"].model == "anthropic:claude-opus-4-6"
    assert "docs_profile" not in calls[-1]
```

- [ ] **Step 3: Run, verify fail.** Run: `cd agent && uv run pytest tests/test_profiles.py tests/test_coder_docs_profile.py tests/test_reviewer_profile.py tests/test_worker.py -q` — Expected: FAIL.

- [ ] **Step 4: Implement.** Profile factories (each):

```python
def get_coder_profile(model: str | None = None) -> CoderProfile:
    return CoderProfile(
        assistant_id="clipse-coder",
        model=model if model is not None else "anthropic:claude-sonnet-4-6",
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=_SHELL_ALLOW_LIST,
    )
```

(Consider a module-level `_DEFAULT_MODEL` const per file to keep the literal single-sourced.) `worker.py` — add args:

```python
parser.add_argument("--model", default="")
parser.add_argument("--docs-model", default="")
```

`_run_lane_graph` gains `extra_kwargs: dict[str, Any] | None = None` and calls `build_graph(checkpointer=checkpointer, **(extra_kwargs or {}))`. `_dispatch`:

```python
if lane == Lane.coder:
    return await _run_lane_graph(args, build_coder_graph, lane=Lane.coder, extra_kwargs={
        "profile": get_coder_profile(args.model or None),
        "docs_profile": get_coder_docs_profile(args.docs_model or None),
    })
if lane == Lane.reviewer:
    return await _run_lane_graph(args, build_reviewer_graph, lane=Lane.reviewer, extra_kwargs={
        "profile": get_reviewer_profile(args.model or None),
    })
```

- [ ] **Step 5: Run, verify pass.** Run: `cd agent && uv run pytest tests/ -q` — Expected: PASS (baseline was 45 passed in the four touched files).

- [ ] **Step 6: Commit.**

```bash
git add agent/src/clipse_agent/profiles/ agent/src/clipse_agent/worker.py agent/tests/
git commit -m "feat(worker): thread configured model into lane profiles"
```

---

### Task 5: Python DAC — `openai_codex` pre-build

**Files:**
- Modify: `agent/src/clipse_agent/dac.py:82-121` (`build_coder_agent`)
- Test: `agent/tests/test_dac.py`

**Interfaces:**
- Consumes: `profile.model` (a `provider:model` string).
- Produces: for `openai_codex`, passes a pre-built `BaseChatModel` to `create_cli_agent`; else passes the string.

- [ ] **Step 1: Write failing tests.** In `test_dac.py`, extend the `fake_create_cli_agent` capture (`captured["model"]`):

```python
def test_build_coder_agent_codex_prebuilds_model_object(monkeypatch):
    sentinel = object()
    monkeypatch.setattr(dac, "create_model", lambda spec: SimpleNamespace(model=sentinel))
    captured = {}
    monkeypatch.setattr(dac, "create_cli_agent", lambda model, aid, **kw: captured.update(model=model) or (object(), object()))
    dac.build_coder_agent(_profile(model="openai_codex:gpt-5.5"), None, "/tmp")
    assert captured["model"] is sentinel  # object, never the string

def test_build_coder_agent_anthropic_passes_string(monkeypatch):
    called = {"create_model": False}
    monkeypatch.setattr(dac, "create_model", lambda spec: called.__setitem__("create_model", True))
    captured = {}
    monkeypatch.setattr(dac, "create_cli_agent", lambda model, aid, **kw: captured.update(model=model) or (object(), object()))
    dac.build_coder_agent(_profile(model="anthropic:claude-sonnet-4-6"), None, "/tmp")
    assert captured["model"] == "anthropic:claude-sonnet-4-6"
    assert called["create_model"] is False

def test_build_coder_agent_wraps_create_model_failure(monkeypatch):
    monkeypatch.setattr(dac, "create_model", lambda spec: (_ for _ in ()).throw(RuntimeError("no creds")))
    with pytest.raises(dac.DacError) as ei:
        dac.build_coder_agent(_profile(model="openai_codex:gpt-5.5"), None, "/tmp")
    assert isinstance(ei.value.__cause__, RuntimeError)
```

- [ ] **Step 2: Run, verify fail.** Run: `cd agent && uv run pytest tests/test_dac.py -q` — Expected: FAIL.

- [ ] **Step 3: Implement.** New imports at top of `dac.py`: `from deepagents_code.config import create_model` and `from deepagents_code.model_config import CODEX_PROVIDER`. Rewrite the `try` body of `build_coder_agent`:

```python
try:
    model: str | Any = profile.model
    if profile.model.split(":", 1)[0] == CODEX_PROVIDER:
        # init_chat_model can't resolve the DAC-only "openai_codex" provider
        # (raises ValueError); DAC's create_model wires the on-disk OAuth
        # token store and returns a ready BaseChatModel to hand over instead.
        model = create_model(profile.model).model
    return create_cli_agent(
        model,
        profile.assistant_id,
        system_prompt=profile.system_prompt,
        interactive=False,
        auto_approve=False,
        interrupt_shell_only=True,
        shell_allow_list=list(profile.shell_allow_list),
        enable_ask_user=True,
        enable_shell=True,
        checkpointer=checkpointer,
        cwd=cwd,
    )
except Exception as exc:
    raise DacError(
        f"failed to build DAC agent for assistant_id={profile.assistant_id!r}: {exc}"
    ) from exc
```

- [ ] **Step 4: Run, verify pass.** Run: `cd agent && uv run pytest tests/test_dac.py -q` — Expected: PASS. Then `make test` (full gate).

- [ ] **Step 5: Commit.**

```bash
git add agent/src/clipse_agent/dac.py agent/tests/test_dac.py
git commit -m "feat(dac): pre-build model object for openai_codex provider"
```

---

### Task 6: Docs — AGENTS.md notes + regen check

**Files:**
- Modify: `AGENTS.md` (Conventions / Open follow-ups)

- [ ] **Step 1: Update AGENTS.md.** Add: (a) per-lane model config is set in `clipse.yaml`'s `models:` block (keys `coder`/`coder_docs`/`reviewer`), defaulting to the current anthropic strings; (b) `openai_codex` needs a one-time ChatGPT sign-in on the dispatcher host as the same OS user (token at `~/.deepagents/.state/chatgpt-auth.json`, reachable via the already-allow-listed `HOME`) — a Phase-2 gate alongside `ANTHROPIC_API_KEY`/`gh`; (c) the gotcha: `create_cli_agent(str)` routes through `init_chat_model`, which raises on `openai_codex`, so `dac.build_coder_agent` pre-builds a `BaseChatModel` via `create_model` for that provider only. Note the OAuth-token-in-coder's-shell-reach residual risk from the design doc's Risks.
- [ ] **Step 2: Verify no codegen drift.** Run: `make codegen && git diff --exit-code` — Expected: no diff (this change touches no schema).
- [ ] **Step 3: Full gate.** Run: `make test && make lint` — Expected: PASS.
- [ ] **Step 4: Commit.**

```bash
git add AGENTS.md
git commit -m "docs: per-lane model config + openai_codex auth gate"
```

---

## Self-review notes

- **Spec coverage:** config schema (T1), Go threading (T2/T3), Python worker+profiles (T4), DAC fix (T5), docs + auth gate (T6). Backward-compat defaults land in both T1 (Go) and T4 (Python). Auth prerequisite is operational (documented in T6), not code. `MissingCredentialsError` reclassification and the OAuth-token-reach hardening are explicitly out of scope (design doc Open questions / Risks) — no task, by design.
- **Manual prerequisite (not a code task):** before a live codex run, the operator performs the one-time ChatGPT sign-in (`uv --project agent run dcode` → `/auth` → openai_codex) as the dispatcher's OS user.
- **Rollout:** ship on a branch as a draft PR; `make test` (`test-go` + `test-py`) green + `make codegen` no-drift are the merge gate.
