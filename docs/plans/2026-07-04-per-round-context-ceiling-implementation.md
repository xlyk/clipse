# Per-round context ceiling + tighter auto-compaction — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** stop coder turns tripping the token ceiling on cumulative spend. Redefine clipse's per-run token ceiling from *cumulative sum across rounds* to *peak per-round context (input tokens)*, and make DAC's already-installed auto-summarizer compact sooner.

**Background (verified — see `.superpowers/sdd/autocompaction-spike.md`):** DAC auto-compaction is ALREADY installed (`create_deep_agent` unconditionally adds it, `deepagents/graph.py:779`). Two real issues: (1) its trigger is `0.85 × model.profile["max_input_tokens"]` per round — loose on big-window models; (2) `dac.py:drive_turn` sums `input_tokens` across every round and trips when the *cumulative* sum exceeds `max_tokens`, so a long (compacted) turn still balloons past a fixed ceiling. This plan fixes both. SAFETY invariant (`auto_approve=False, interrupt_shell_only=True, shell_allow_list, enable_ask_user=True`) stays byte-for-byte — no `create_deep_agent` fork.

**Tech Stack:** Python 3.13 + uv (pydantic v2, deepagents-code 0.1.22, langgraph); Go 1.25 (config only).

## Global Constraints

- **SAFETY:** the `create_cli_agent(...)` call in `dac.build_coder_agent` keeps its exact args. No fork of `create_deep_agent`. A regression test must assert a `SummarizationMiddleware` is present in the compiled stack (pins the "already installed" assumption against DAC upgrades).
- Ceiling semantics change is **surgical**: keep the config key `max_tokens_per_run` and the `--max-tokens` flag; only redefine what `drive_turn` compares against + the doc + the tuned values. `WorkerResult.tokens` still reports cumulative in/out (reporting unchanged).
- Python: pydantic v2; `model if x is not None else default` idiom; ruff clean; TDD.
- Go: table-driven stdlib tests. `make test` is the gate.
- Commits: Conventional, lowercase, no trailing period, no AI signature. One concern per commit.

## Locked design decisions

- **Ceiling metric = peak per-round `input_tokens`.** In `drive_turn`, track the max single-round input; trip `token_ceiling_exceeded` when a round's input exceeds `max_tokens` (not the cumulative sum). Post-compaction, a round only exceeds this if compaction failed → a genuine runaway. Output tokens are not context → excluded from the guard.
- **Trigger driver = `model.profile["max_input_tokens"]`** set to `context_window_tokens` (default 200_000 → ~170K/round compaction) before `create_cli_agent`.
- **Tuned values:** `max_tokens_per_run` (now "max per-round context") default → 400_000 (> the 170K trigger, so compaction has headroom and the guard only catches real runaways). `context_window_tokens` → 200_000.
- Backstops for total work remain `max_runtime_s` + `turn_cap`.

## File Structure / order

1. `agent/src/clipse_agent/dac.py` — `drive_turn` ceiling metric (core); `build_coder_agent` trigger-lower + nudge.
2. `agent/src/clipse_agent/profiles/{coder,reviewer}.py` — `context_window_tokens` field.
3. `internal/config/config.go` + `configs/clipse.example.yaml` — redefine/re-tune `max_tokens_per_run` doc + default.
4. `AGENTS.md` — document the semantics + the "compaction already installed" note.
5. Live configs (`~/Code/clipse-smoke/clipse.yaml`, `reflex.clipse.yaml`) — re-set the value (operator step, not a repo commit).

---

### Task 1: `drive_turn` — per-round context ceiling

**Files:** Modify `agent/src/clipse_agent/dac.py` (`drive_turn`, the accumulation/break at ~239-245; `_accumulate_message_chunk` returns per-chunk `(in,out)`). Test: `agent/tests/test_dac.py`.

**Interfaces:** `drive_turn(..., max_tokens)` unchanged signature; semantics of `max_tokens` → per-round input cap. `DacTurnResult.token_ceiling_exceeded` now means "a single round's context exceeded `max_tokens`."

- [ ] **Step 1: Failing tests.** In `test_dac.py`, drive a fake `astream` (reuse the existing fake stream harness) with:
  - (a) many rounds each with `input_tokens` below `max_tokens` whose SUM exceeds `max_tokens` → assert `token_ceiling_exceeded is False` (the old cumulative behavior would trip — this proves the new per-round semantics).
  - (b) one round with `input_tokens > max_tokens` → assert `token_ceiling_exceeded is True` and the stream is abandoned.
  - (c) `WorkerResult`/`DacTurnResult` still reports cumulative `tokens_in`/`tokens_out` (reporting unchanged).
- [ ] **Step 2: Run, verify fail.** `cd agent && uv run pytest tests/test_dac.py -q`.
- [ ] **Step 3: Implement.** In `drive_turn`'s loop: keep accumulating `tokens_in += turn_in`, `tokens_out += turn_out` (for reporting). Add `max_round_in = max(max_round_in, turn_in)`. Replace the break condition:

```python
# was: if max_tokens is not None and (tokens_in + tokens_out) > max_tokens:
if max_tokens is not None and turn_in > max_tokens:
    token_ceiling_exceeded = True
    break
```

Update the docstring: the ceiling is now the largest single round's input (context) tokens — a post-compaction runaway guard — not cumulative turn spend.

- [ ] **Step 4: Run, verify pass.** `cd agent && uv run pytest tests/test_dac.py -q`.
- [ ] **Step 5: Commit.** `git add agent/src/clipse_agent/dac.py agent/tests/test_dac.py && git commit -m "fix(dac): per-round context ceiling instead of cumulative turn sum"`

---

### Task 2: trigger-lower + compact nudge + summarizer regression test

**Files:** Modify `agent/src/clipse_agent/dac.py` (`build_coder_agent`), `profiles/coder.py`, `profiles/reviewer.py`. Test: `test_dac.py`, `test_profiles.py`/`test_reviewer_profile.py`.

**Interfaces:** `CoderProfile`/`ReviewerProfile` gain `context_window_tokens: int | None = 200_000`. `build_coder_agent` sets `model.profile["max_input_tokens"]` from it before `create_cli_agent`.

- [ ] **Step 1: Failing tests.**
  - profiles: `get_coder_profile().context_window_tokens == 200000`; override respected.
  - dac: with a codex/profile-carrying model, `build_coder_agent` sets `model.profile["max_input_tokens"] == profile.context_window_tokens` on the instance passed to `create_cli_agent` (monkeypatch `create_model` to return a sentinel model with a `.profile` dict; assert it's mutated; assert `create_cli_agent` still receives the SAFETY args unchanged).
  - regression: build a real (or lightly-faked) agent and assert a middleware named `"SummarizationMiddleware"` is in the compiled graph's middleware (pins "compaction installed"). If inspecting the compiled Pregel is impractical, assert via `create_deep_agent`'s documented stack — keep it a real check, not a mock.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.**
  - Add `context_window_tokens: int | None = 200_000` to both profile dataclasses (frozen; dict/int field fine); thread through the factories.
  - In `build_coder_agent`: after obtaining the model object, if `profile.context_window_tokens` is set, ensure the model came via `create_model` (so it has a `.profile`) and set `m.profile = {**(m.profile or {}), "max_input_tokens": profile.context_window_tokens}` before `create_cli_agent`. Keep the codex-or-params routing from before; when `context_window_tokens` is set, also route through `create_model` so a profile exists. Do NOT change any `create_cli_agent` SAFETY arg.
  - Append a one-line `compact_conversation` nudge to the coder `system_prompt` (Option 5): e.g. "If your context grows large, call `compact_conversation` to summarize older history before continuing." (coder + docs prompts; reviewer optional.)
- [ ] **Step 4: Run, verify pass.** `cd agent && uv run pytest -q`.
- [ ] **Step 5: Commit.** `git add agent/src/clipse_agent/dac.py agent/src/clipse_agent/profiles/ agent/tests/ && git commit -m "feat(dac): lower auto-summarizer trigger + compact nudge"`

---

### Task 3: config semantics + docs

**Files:** Modify `internal/config/config.go` (the `max_tokens_per_run` doc comment + any default), `configs/clipse.example.yaml`, `AGENTS.md`. Test: `internal/config/config_test.go` if the default changes.

- [ ] **Step 1:** Update `max_tokens_per_run`'s doc comment (config.go + example.yaml) to define it as **the per-round context (input-token) ceiling** — the largest single round's input that the worker will allow before blocking `capability`; not a cumulative-spend cap. Set the example/default value to `400000`. If there's a Go default constant, update it + its test.
- [ ] **Step 2:** `AGENTS.md`: note (a) DAC auto-compaction is built-in (create_deep_agent) and clipse tunes its trigger via `context_window_tokens`; (b) `max_tokens_per_run` is a per-round context guard (post-compaction runaway), with `max_runtime_s`/`turn_cap` as the total-work backstops.
- [ ] **Step 3:** `make codegen && git diff --exit-code` (no schema touched → no drift); `make test && make lint`.
- [ ] **Step 4: Commit.** `git add internal/config/config.go configs/clipse.example.yaml AGENTS.md internal/config/config_test.go && git commit -m "docs: redefine max_tokens_per_run as per-round context ceiling"`

---

## Post-merge (operator, not a repo commit)
- Re-set `max_tokens_per_run` in `~/Code/clipse-smoke/clipse.yaml` and `reflex.clipse.yaml` from 5000000 → ~400000 (now a per-round ceiling). Rebuild + restart the reflex dispatcher to pick up the new binary + config.

## Self-review notes
- Ceiling change is behavior-visible: verify Task 1's tests prove cumulative-no-longer-trips AND per-round-trips.
- Regression test (Task 2) guards the "compaction already installed" assumption against DAC upgrades (spike open-risk: version coupling).
- Open risk (spike): summary-generation `.invoke` may surface tokens on the stream — Task 1's per-round metric makes this harmless (a 4K summary round is far below the 400K guard), but note it.
