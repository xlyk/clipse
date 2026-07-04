# Per-lane model params (reasoning effort etc.) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. Steps use checkbox (`- [ ]`) syntax.

**Goal:** let each clipse lane pass arbitrary DAC model-construction params (e.g. `reasoning_effort` for codex, `thinking` budget for anthropic) via `clipse.yaml`, threaded into `create_model(extra_kwargs=...)`.

**Architecture:** A new `model_params:` block in `clipse.yaml` (keyed by profile identity `coder`/`coder_docs`/`reviewer`, each an arbitrary map) flows Go `config.ModelParams` → `dispatcher.spawnAttempt` → `WorkerSpec.ModelParams`/`.DocsModelParams` (JSON strings) → `--model-params`/`--docs-model-params` argv → Python worker → profile `model_params` dict → `dac.build_coder_agent`, which routes a lane through DAC's `create_model(profile.model, extra_kwargs=profile.model_params)` whenever params are present (or the provider is `openai_codex`), else keeps the plain-string path. Mirrors the just-shipped per-lane `models:` feature.

**Tech Stack:** Go 1.25 (stdlib testing, yaml.v3), Python 3.13 + uv (pydantic v2, deepagents-code 0.1.22).

**Depends on:** the merged per-lane-models feature (`docs/design/2026-07-03-configurable-per-lane-models.md`, `docs/plans/2026-07-03-configurable-per-lane-models-implementation.md`). Read those — this reuses the exact same seams (`config.Models`→`WorkerSpec`→`workerArgs`→`worker._dispatch`→profiles→`dac.build_coder_agent`).

## Global Constraints

- Go: wrap errors (`%w`); table-driven stdlib tests (no testify); `Tick` stays `go test -race` clean.
- **Kernel stays LLM-free**: `model_params` is an **opaque passthrough** — Go marshals the map to JSON and threads it; it never interprets or validates param *semantics* (only that it's a JSON object).
- Python: `model if model is not None else default` idiom preserved; ruff clean; pydantic v2.
- **Precedence**: DAC applies `~/.deepagents/config.toml` provider params first, then clipse's `extra_kwargs` (per-lane `model_params`) on top — so a lane's `model_params` overrides the machine-global config.toml default; a lane with no `model_params` inherits config.toml.
- SAFETY block in `create_cli_agent` unchanged.
- Defaults: omitted `model_params` → no extra kwargs → behavior identical to today (anthropic string path stays for lanes with no params + non-codex provider).
- Commits: Conventional Commits, lowercase, no trailing period, no AI signature. Never `git add -A`. `make test` is the gate.

## File Structure

Same files the models feature touched:
- `internal/config/config.go` — `ModelParams`/`rawModelParams` types + `Config.ModelParams` + `Load` wiring (no defaulting, no semantic validation).
- `configs/clipse.example.yaml` — `model_params:` example block.
- `internal/spawn/spawn.go` + `local.go` — `WorkerSpec.ModelParams`/`.DocsModelParams` (JSON strings) + `--model-params`/`--docs-model-params` flags.
- `dispatcher/spawn.go` — resolve lane → params JSON, wire into the `WorkerSpec` literal (extend `modelsFor` or add `modelParamsFor`).
- `agent/src/clipse_agent/worker.py` — `--model-params`/`--docs-model-params` args (JSON) → dict → profiles.
- `agent/src/clipse_agent/profiles/{coder,reviewer}.py` — `model_params: dict | None` field + factory param.
- `agent/src/clipse_agent/dac.py` — route through `create_model(extra_kwargs=...)` when params present or codex.
- `AGENTS.md` — document the block + precedence.

Order: config → spawn → dispatcher → python worker/profiles → dac → docs.

---

### Task 1: Go config — `model_params:` block

**Files:** Modify `internal/config/config.go` (near `Models` `:127`, `rawModels` `:227`, `Load` `:289`), `configs/clipse.example.yaml`. Test: `internal/config/config_test.go`.

**Interfaces:** Produces `config.ModelParams{Coder, CoderDocs, Reviewer map[string]any}` on `cfg.ModelParams`.

- [ ] **Step 1: Failing tests.** In `config_test.go`: assert a `model_params:` block parses (incl. a nested value like `reasoning_effort: high` on coder and `thinking: {type: enabled, budget_tokens: 10000}` on reviewer); assert omitted `model_params` → nil maps for all three; assert per-key independence (only `coder` set → others nil).

```go
func TestLoad_ModelParams(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML+`
model_params:
  coder: { reasoning_effort: high }
  reviewer: { thinking: { type: enabled, budget_tokens: 10000 } }
`)
	if cfg.ModelParams.Coder["reasoning_effort"] != "high" {
		t.Errorf("coder reasoning_effort = %v", cfg.ModelParams.Coder["reasoning_effort"])
	}
	if cfg.ModelParams.CoderDocs != nil {
		t.Errorf("coder_docs should be nil, got %v", cfg.ModelParams.CoderDocs)
	}
	th, _ := cfg.ModelParams.Reviewer["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Errorf("reviewer thinking = %v", cfg.ModelParams.Reviewer["thinking"])
	}
}
```

- [ ] **Step 2: Run, verify fail.** `go test ./internal/config/ -run ModelParams -v` — FAIL (unknown field).
- [ ] **Step 3: Implement.**

```go
type ModelParams struct {
	Coder     map[string]any `yaml:"coder"`
	CoderDocs map[string]any `yaml:"coder_docs"`
	Reviewer  map[string]any `yaml:"reviewer"`
}
type rawModelParams struct {
	Coder     map[string]any `yaml:"coder"`
	CoderDocs map[string]any `yaml:"coder_docs"`
	Reviewer  map[string]any `yaml:"reviewer"`
}
```

Add `ModelParams ModelParams `yaml:"model_params"`` to `Config`; `ModelParams *rawModelParams `yaml:"model_params"`` to `rawConfig`. In `Load`, copy the maps through (nil when the raw sub-struct or key is absent — no defaulting). No `validate` entry: params are opaque (kernel LLM-free); a non-mapping YAML value already surfaces as a yaml decode error at Load.

- [ ] **Step 4: Example config.** Add after the `models:` block in `configs/clipse.example.yaml`:

```yaml
# model_params: optional per-lane kwargs passed to the model constructor
# (DAC create_model extra_kwargs). Overrides ~/.deepagents/config.toml provider
# params. Opaque to the kernel. Examples: codex reasoning effort, anthropic
# extended-thinking budget. Omit a lane to inherit config.toml / provider default.
# model_params:
#   coder: { reasoning_effort: high }
#   coder_docs: { reasoning_effort: low }
#   reviewer: { thinking: { type: enabled, budget_tokens: 10000 } }
```

- [ ] **Step 5: Run, verify pass.** `go test ./internal/config/ -v`.
- [ ] **Step 6: Commit.** `git add internal/config/config.go configs/clipse.example.yaml internal/config/config_test.go && git commit -m "feat(config): add per-lane model_params passthrough"`

---

### Task 2: Go spawn — `WorkerSpec` JSON fields + flags

**Files:** Modify `internal/spawn/spawn.go` (WorkerSpec, near `Model`/`DocsModel`), `internal/spawn/local.go` (workerArgs). Test: `internal/spawn/argv_test.go`.

**Interfaces:** Produces `WorkerSpec.ModelParams string` (JSON), `.DocsModelParams string`; `workerArgs` emits `--model-params=<json>` / `--docs-model-params=<json>` when non-empty.

- [ ] **Step 1: Failing tests.** Add `TestWorkerArgs` cases: `ModelParams: "{\"reasoning_effort\":\"high\"}"` → argv contains `--model-params={"reasoning_effort":"high"}`; docs-params only; both empty → neither flag.
- [ ] **Step 2: Run, verify fail.** `go test ./internal/spawn/ -run TestWorkerArgs -v`.
- [ ] **Step 3: Implement.** Add to `WorkerSpec` (comments mirror `Model`):

```go
ModelParams     string // JSON object of extra model-construction kwargs; empty = none
DocsModelParams string // JSON object for the coder docs sub-step; empty = none
```

In `workerArgs`, after the `--docs-model` conditional:

```go
if spec.ModelParams != "" {
	args = append(args, "--model-params="+spec.ModelParams)
}
if spec.DocsModelParams != "" {
	args = append(args, "--docs-model-params="+spec.DocsModelParams)
}
```

- [ ] **Step 4: Run, verify pass.** `go test ./internal/spawn/ -v`.
- [ ] **Step 5: Commit.** `git add internal/spawn/spawn.go internal/spawn/local.go internal/spawn/argv_test.go && git commit -m "feat(spawn): thread model-params json into worker argv"`

---

### Task 3: Go dispatcher — resolve + wire per-lane params

**Files:** Modify `dispatcher/spawn.go` (near `modelsFor`, the `WorkerSpec` literal). Test: `dispatcher/models_test.go` (extend) or new `dispatcher/model_params_test.go`.

**Interfaces:** Consumes `config.ModelParams`, `WorkerSpec.ModelParams/.DocsModelParams`. Produces `modelParamsFor(lane) (params, docsParams string)` returning compact JSON (`""` when the lane's map is nil/empty).

- [ ] **Step 1: Failing test.** Extend the `dispatcher/models_test.go` integration test (tick a real Dispatcher with `cfg.ModelParams` set): assert `fakeSpawner.Specs()[0].ModelParams` is the JSON of the coder map, `.DocsModelParams` is the coder_docs map's JSON; reviewer claim → `.ModelParams` = reviewer map JSON, `.DocsModelParams == ""`. Include a case where a lane has nil params → `""` (flag omitted).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** Add a helper (JSON-marshal via `encoding/json`; nil/empty map → `""`):

```go
func (d *Dispatcher) modelParamsFor(lane string) (params, docsParams string) {
	enc := func(m map[string]any) string {
		if len(m) == 0 {
			return ""
		}
		b, err := json.Marshal(m)
		if err != nil {
			d.log.Warn("marshal model_params", "lane", lane, "err", err)
			return ""
		}
		return string(b)
	}
	switch lane {
	case string(contract.LaneCoder):
		return enc(d.cfg.ModelParams.Coder), enc(d.cfg.ModelParams.CoderDocs)
	case string(contract.LaneReviewer):
		return enc(d.cfg.ModelParams.Reviewer), ""
	default:
		return "", ""
	}
}
```

(Match the file's actual logger field name — check `modelsFor`/surrounding code for how `d` logs; if there's no logger handle, drop the Warn and return `""` on error.) In `spawnAttempt`, alongside `model, docsModel := d.modelsFor(lane)` add `modelParams, docsModelParams := d.modelParamsFor(lane)` and set `ModelParams: modelParams, DocsModelParams: docsModelParams,` in the `WorkerSpec` literal.

- [ ] **Step 4: Run, verify pass.** `go test ./dispatcher/ -v && go test -race ./...`.
- [ ] **Step 5: Commit.** `git add dispatcher/spawn.go dispatcher/*_test.go && git commit -m "feat(dispatcher): resolve per-lane model_params into workerspec"`

---

### Task 4: Python worker + profiles — parse & thread params

**Files:** Modify `agent/src/clipse_agent/worker.py`, `profiles/coder.py`, `profiles/reviewer.py`. Test: `agent/tests/test_worker.py`, `test_profiles.py`, `test_coder_docs_profile.py`, `test_reviewer_profile.py`.

**Interfaces:** Produces `get_coder_profile(model=None, model_params=None)` (+ docs, reviewer); `CoderProfile.model_params: dict | None`; worker args `--model-params`, `--docs-model-params` (JSON strings, default `""`).

- [ ] **Step 1: Failing tests.** Profiles: `get_coder_profile(model_params={"reasoning_effort": "high"}).model_params == {"reasoning_effort": "high"}`; default `None` when omitted. Worker: `--model-params='{"reasoning_effort":"high"}'` on a coder run → the coder profile passed to `build_coder_graph` carries `model_params={"reasoning_effort":"high"}`; `--docs-model-params` → docs profile; reviewer run threads `--model-params` into the reviewer profile; empty/absent → `None` (parse `args.model_params or "null"`? — use: `json.loads(x) if x else None`).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** Add a frozen-dataclass field `model_params: dict[str, Any] | None = None` to `CoderProfile` and `ReviewerProfile` (a dict field on a frozen dataclass is fine — frozen blocks reassignment, not mutation; matches how the profile already holds structured data). Factories accept `model_params: dict[str, Any] | None = None` and pass it through. `worker.py`:

```python
parser.add_argument("--model-params", default="")
parser.add_argument("--docs-model-params", default="")
...
def _parse_params(raw: str) -> dict[str, Any] | None:
    return json.loads(raw) if raw else None
```

In `_dispatch`: `get_coder_profile(args.model or None, model_params=_parse_params(args.model_params))`, `get_coder_docs_profile(args.docs_model or None, model_params=_parse_params(args.docs_model_params))`, `get_reviewer_profile(args.model or None, model_params=_parse_params(args.model_params))`.

- [ ] **Step 4: Run, verify pass.** `cd agent && uv run pytest -q`.
- [ ] **Step 5: Commit.** `git add agent/src/clipse_agent/ agent/tests/ && git commit -m "feat(worker): thread per-lane model_params into profiles"`

---

### Task 5: DAC — apply params via `create_model(extra_kwargs=...)`

**Files:** Modify `agent/src/clipse_agent/dac.py` (`build_coder_agent`). Test: `agent/tests/test_dac.py`.

**Interfaces:** Consumes `profile.model` + `profile.model_params`.

- [ ] **Step 1: Failing tests.** (1) anthropic profile **with** `model_params={"thinking": {...}}` → `create_model` called with `extra_kwargs == profile.model_params`, and the returned `.model` object (not the string) passed to `create_cli_agent`; (2) codex profile with `model_params={"reasoning_effort":"low"}` → `create_model` called with that `extra_kwargs`; (3) anthropic profile with **no** params → string path, `create_model` NOT called (unchanged); (4) codex with no params → `create_model` called with `extra_kwargs` None/empty (existing behavior).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** Replace the codex-only branch condition:

```python
provider = profile.model.split(":", 1)[0]
use_create_model = provider == CODEX_PROVIDER or bool(profile.model_params)
model: str | Any = profile.model
if use_create_model:
    model = create_model(profile.model, extra_kwargs=profile.model_params or None).model
return create_cli_agent(model, profile.assistant_id, ...)  # SAFETY block unchanged
```

(`extra_kwargs` is `create_model`'s documented param. `profile.model_params or None` passes `None` for empty/absent.)

- [ ] **Step 4: Run, verify pass.** `cd agent && uv run pytest tests/test_dac.py -q` then `make test`.
- [ ] **Step 5: Commit.** `git add agent/src/clipse_agent/dac.py agent/tests/test_dac.py && git commit -m "feat(dac): apply per-lane model_params via create_model extra_kwargs"`

---

### Task 6: Docs + gate

**Files:** Modify `AGENTS.md`.

- [ ] **Step 1: Update AGENTS.md.** Document: per-lane `model_params:` (keys `coder`/`coder_docs`/`reviewer`, opaque map → DAC `create_model` extra_kwargs); precedence (`~/.deepagents/config.toml` provider params < clipse per-lane `model_params`); codex effort via `reasoning_effort`; that any lane with `model_params` now routes through `create_model` (not just codex). Note the kernel stays LLM-free (opaque passthrough, no semantic validation).
- [ ] **Step 2: No codegen drift.** `make codegen && git diff --exit-code`.
- [ ] **Step 3: Full gate.** `make test && make lint`.
- [ ] **Step 4: Commit.** `git add AGENTS.md && git commit -m "docs: per-lane model_params + effort precedence"`

---

## Self-review notes
- Coverage: config (T1), spawn (T2), dispatcher (T3), python worker/profiles (T4), dac (T5), docs (T6) — mirrors the models feature 1:1.
- Interaction with config.toml (feature A): config.toml provides the global default effort; per-lane `model_params` override via `extra_kwargs` (DAC precedence config.toml < extra_kwargs). A lane with no `model_params` inherits config.toml.
- No new secrets, no schema change (no codegen drift). Anthropic reviewer only routes through `create_model` when it has `model_params`, so the untouched-string path stays the default for paramless lanes.
