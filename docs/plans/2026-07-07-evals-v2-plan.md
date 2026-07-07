# Agent Evals v2 Implementation Plan (DRAFT)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Workstream B on top of the merged v1 eval suite (`f801d46`): per-run results tooling, the L2 coder→reviewer→rework convergence harness (the deliverable metric: cost-per-approved-PR), the first LLM-judge case (D3 docs accuracy), a smoke-side R5 placement check, and two small harness/worker fixes.

**Architecture:** Everything lives on the v1 foundation — pytest cases in `agent/evals/` driving the real graphs against fixture repos + the `gh` shim, deterministic graders first. v2 adds: (1) a session-scoped per-run results file with pass/fail rows and a `make eval-report` summarizer; (2) `run_convergence_loop` in `harness.py` that drives the FULL coder→reviewer→rework loop in-process, mirroring the dispatcher's rework mechanics exactly (fresh `task_text` carrying the reviewer's `summary` — the same value `store.LatestReworkFeedback` threads into `CLIPSE_REVIEW_FEEDBACK` — on the SAME raw thread_id, with a shared per-issue checkpoint DB opened per turn exactly like `worker.py`); (3) a minimal `judge.py` built on `deepagents_code.config.create_model` (already a direct dep — no new dependency) pinned to `anthropic:claude-haiku-4-5`, used by exactly ONE case; (4) a report-only placement script hooked into `scripts/smoke/smoke.sh verify`.

**Tech Stack:** pytest (existing), real `git`, the v1 `gh` shim, `asyncio.run` (no pytest-asyncio), `AsyncSqliteSaver` (already a direct dep via v1's pyproject fix), `deepagents_code.config.create_model` for the judge, stdlib-only report/placement scripts.

## Global Constraints

- Python `>=3.13`, uv-managed (`cd agent && uv run ...`). Async graphs driven with plain `asyncio.run(...)` — never pytest-asyncio.
- **No new dependencies.** The judge uses `deepagents_code.config.create_model(...)` — the public model factory of a DIRECT dep that `dac.py` already imports. `anthropic` / `langchain-anthropic` exist only transitively, and this repo treats importing a transitive as a bug (v1 plan promoted `langgraph-checkpoint-sqlite` for exactly that reason). If `create_model` turns out unusable for a bare judge call, STOP and flag it as an open question — do not `uv add anthropic` silently.
- `make test` (and CI) stays green and never collects evals (`pyproject.toml` `testpaths = ["tests"]` is untouched). Live cases skip cleanly without credentials; the judge has its own skip guard (pinned to anthropic even under a codex matrix run).
- Deterministic graders first; the LLM judge appears in exactly ONE case (D3). Every live case notes its expected cost.
- Kernel (Go) untouched except zero lines — the only production-code change is `agent/src/clipse_agent/graphs/reviewer.py` (Task 6), with unit tests, one clean commit.
- Honest assertions: convergence cases assert `rounds_to_done <= N` with the recorded metric — never a hard `== 2` that flakes on a good round-1.
- Ruff-clean (`make lint` covers `agent/evals/`). Commits: conventional, lowercase, no trailing period, one concern each. Branch `feat/agent-evals-v2`. Never `git add -A` at the repo root (untracked `.superpowers/` must never be committed).
- Cost: v1 full suite is a few dollars; v2 adds the L2 cases (~4–6 live turns each, reviewer turns on opus) — a full run lands in the $10–20 range on default models. `-k` filters keep single-case iteration cheap.

## Why each item exists (motivation index)

| Item | Motivation / finding |
|---|---|
| Per-run results file | `latest.jsonl` is append-forever across sessions — no way to compare runs or track the R7 nondeterminism metric "across runs" as its own docstring demands. |
| R7 under-recording | `test_r7_verdict_consistency_on_clean_pr` runs 3 consistency turns that write NO rows, then burns a 4th opus turn purely to have a `WorkerResult` to record. Fix records each run and deletes the 4th. |
| `make eval-report` | Rows exist but nothing summarizes them; budget tuning ("tune against results/latest.jsonl") is currently manual jq. |
| L2 convergence harness + L2a/L2b | The deliverable metric is cost-per-approved-PR. v1 tests each lane's single turn; nothing proves the loop converges, or measures rounds/tokens to done. Deferred from v1 explicitly. |
| R6 summary actionability | AGENTS.md open follow-up: rework feedback carries the reviewer's rollup `summary`, which can go vague — convergence can hinge on it. L2a asserts the round-2 coder actually addressed round-1 findings (diff changed + planted defect fixed) — measuring whether the summary alone carries enough signal, no separate judge. |
| D3 docs-accuracy judge | D1 asserts `--shout` appears in the README — but not that the sentence around it is TRUE. Accuracy is semantic → first (and only) judge case. |
| R5 placement validity | Deferred from v1: "needs live GitHub to 422 realistically". `post_comments` already degrades out-of-hunk comments to the summary; the placement RATE is only measurable against real GitHub → smoke-side, report-only. |
| gh shim stderr warning | An unhandled shim subcommand currently exits 0 silently — an agent probing `gh pr checks`/`gh repo view` looks like success and the eval author never learns the shim lied. |
| post_comments no-PR grace narrowing | The v1 grace treats ANY `gh pr view` failure as "no PR" — an auth/network failure silently drops every inline finding and the summary. Narrow to the real signal; everything else raises → the kernel's transient retry gets a shot. |
| Nightly/matrix runbook | The codex matrix (`CLIPSE_EVAL_MODEL`) exists but the OAuth prereq + cadence live only in AGENTS.md prose. Docs-only. |

## Deferred (explicitly NOT in this plan)

- `langsmith[pytest]` (judge feedback attached to traces, dataset-driven cases) — the upgrade path once judge usage grows past one case.
- Failure-archive→eval-case pipeline; nightly *automation* (cron infra) — the runbook documents a manual/local cadence only.
- R5 as a pytest eval — would need recorded live-GitHub fixtures; stays smoke-side.
- Worker interrupt-resume evals (`blocked(needs_input)` → human answer → resume) — blocked on the Phase 2/3 resume channel design.
- Threading the reviewer's inline findings (not just the summary) into rework feedback — that is a worker/kernel change AGENTS.md already tracks; L2a *measures* the gap, it does not fix it.

## File Structure

```
agent/
  evals/
    conftest.py                  # modify: per-run file, status-row hook, duration
    harness.py                   # modify: checkpoint_db + lane-namespaced config, run_convergence_loop
    judge.py                     # new: haiku yes/no judge (create_model, no new dep)
    report.py                    # new: summarize newest run (stdlib only)
    test_harness_selftest.py     # modify: recorder/loop/judge-parser/shim selftests (no LLM)
    test_reviewer_evals.py       # modify: R7 records every consistency turn
    test_convergence_evals.py    # new: L2a, L2b
    test_docs_evals.py           # modify: add D3 (judge)
    gh_shim/gh                   # modify: unhandled-subcommand stderr warning + pr-view note
  src/clipse_agent/graphs/reviewer.py   # modify: narrow post_comments no-PR grace
  tests/test_reviewer_graph.py   # modify: unit test for the raise path
scripts/smoke/
  check-placement.py             # new: R5 report-only placement check (stdlib + gh)
  smoke.sh                       # modify: optional non-fatal hook in verify()
Makefile                         # modify: add `eval-report` target
agent/evals/README.md            # modify: results-file docs + model-matrix/cadence runbook
```

---

### Task 0: Branch

- [ ] **Step 1: Create the working branch**

```bash
git checkout -b feat/agent-evals-v2
```

---

### Task 1: Results tooling — per-run file, status rows, R7 recording, `make eval-report`

**Design decision — `latest.jsonl` becomes a symlink, not a truncated file.** Each session writes its own `results/run-<utc-ts>.jsonl` and re-points `latest.jsonl` at it. Rationale: R7's own docstring says the flip count is "the nondeterminism metric to track across runs", and C2's budget comment says "tune against results/latest.jsonl after a few runs" — both need per-run history, which truncation destroys. A symlink keeps `latest.jsonl` as the stable path every doc already references, at zero copy cost. (`results/` is gitignored; a leftover v1 regular `latest.jsonl` is simply unlinked. Windows is a non-goal — this repo targets darwin/linux.)

**Design decision — pass/fail comes from a pytest hook, not `record_result`.** `record_result` runs *before* the case's asserts, so its rows cannot know pass/fail. A `pytest_runtest_makereport` hookwrapper in the evals conftest appends one status row per eval-marked case (`status` ∈ passed/failed/skipped, plus pytest's authoritative wall-clock `duration`). `report.py` joins metric rows and status rows by test id.

**Files:**
- Modify: `agent/evals/conftest.py`
- Modify: `agent/evals/test_reviewer_evals.py` (R7)
- Create: `agent/evals/report.py`
- Modify: `Makefile`, `agent/evals/README.md` (results paragraph)
- Test: `agent/evals/test_harness_selftest.py`

- [ ] **Step 1: Write the failing selftests (no LLM, no cost)**

Append to `agent/evals/test_harness_selftest.py`:

```python
from clipse_agent.contract import Lane, Outcome, Tokens, WorkerResult

import conftest as evals_conftest
import report


def _wr(outcome: Outcome = Outcome.done, *, tokens_in: int = 10, tokens_out: int = 2) -> WorkerResult:
    return WorkerResult(
        run_id="r1", issue_id="EVAL-1", lane=Lane.coder, outcome=outcome,
        summary="s", artifacts=[], thread_id="t", turn_count=1,
        tokens=Tokens(**{"in": tokens_in, "out": tokens_out}),
    )


def test_record_result_writes_to_a_per_run_file_behind_latest_symlink(record_result) -> None:
    record_result(_wr(), marker="selftest-row")
    latest = evals_conftest._RESULTS_DIR / "latest.jsonl"
    assert latest.is_symlink()
    run_file = latest.resolve()
    assert run_file.name.startswith("run-") and run_file.suffix == ".jsonl"
    rows = [json.loads(line) for line in run_file.read_text().splitlines()]
    mine = [r for r in rows if r.get("marker") == "selftest-row"]
    assert len(mine) == 1
    assert mine[0]["outcome"] == "done" and mine[0]["tokens_in"] == 10


def test_report_summarize_joins_metric_and_status_rows(tmp_path: Path) -> None:
    rows = tmp_path / "run.jsonl"
    rows.write_text(
        json.dumps({"test": "evals/x.py::t1", "ts": 1.0, "outcome": "done",
                    "tokens_in": 100, "tokens_out": 5, "turn_count": 1, "block_kind": None}) + "\n"
        + json.dumps({"test": "evals/x.py::t1", "ts": 2.0, "status": "passed", "duration_s": 42.0}) + "\n"
        + json.dumps({"test": "evals/x.py::t2", "ts": 3.0, "status": "skipped", "duration_s": 0.0}) + "\n"
    )
    out = report.summarize(rows)
    assert "t1" in out and "passed" in out and "42" in out and "100" in out
    assert "skipped" in out
    assert "1 passed" in out and "1 skipped" in out
```

(These selftests intentionally write real rows into the gitignored `results/` dir — the session's run file exists anyway, and a selftest row appearing in a run report is harmless and honest.)

- [ ] **Step 2: Run to verify failure**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -k "record_result_writes or report_summarize" -v`
Expected: FAIL (`latest.jsonl` is a regular file / no `report` module).

- [ ] **Step 3: Implement `conftest.py`**

Replace the recording half of `agent/evals/conftest.py` (keep `eval_env` untouched):

```python
"""Eval-suite fixtures: gh shim on PATH, per-run metrics recording."""
from __future__ import annotations

import json
import os
import time
from collections.abc import Callable
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest

from clipse_agent.contract import WorkerResult

_SHIM_DIR = Path(__file__).parent / "gh_shim"
_RESULTS_DIR = Path(__file__).parent / "results"

# Lazily created on first append: one file per pytest session, with
# latest.jsonl re-pointed at it. A symlink (not truncation) because run
# history is the point -- R7's flip count and C2's budget tuning are
# explicitly cross-run metrics; latest.jsonl stays the stable path docs
# reference. Module-global (not a fixture) so the status-row hook below can
# reach it without fixture plumbing.
_RUN_FILE: Path | None = None


def _run_file() -> Path:
    global _RUN_FILE
    if _RUN_FILE is None:
        _RESULTS_DIR.mkdir(exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        _RUN_FILE = _RESULTS_DIR / f"run-{stamp}.jsonl"
        _RUN_FILE.touch()
        latest = _RESULTS_DIR / "latest.jsonl"
        latest.unlink(missing_ok=True)  # also clears a leftover v1 regular file
        latest.symlink_to(_RUN_FILE.name)  # relative target: results/ is self-contained
    return _RUN_FILE


def _append_row(row: dict[str, Any]) -> None:
    with _run_file().open("a") as f:
        f.write(json.dumps(row) + "\n")


@pytest.fixture
def record_result(request: pytest.FixtureRequest) -> Callable[..., None]:
    """Append one JSONL metrics row per recorded result to this session's run file."""

    def _record(result: WorkerResult, **extra: Any) -> None:
        _append_row({
            "test": request.node.nodeid,
            "ts": time.time(),
            "outcome": result.outcome.value,
            "block_kind": result.block_kind.value if result.block_kind else None,
            "tokens_in": result.tokens.in_,
            "tokens_out": result.tokens.out,
            "turn_count": result.turn_count,
            **extra,
        })

    return _record


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item: pytest.Item, call: pytest.CallInfo):
    """Append one status row per eval-marked case: pass/fail/skip + pytest's
    authoritative wall-clock duration. record_result rows run BEFORE the
    case's asserts and cannot know pass/fail; this hook can."""
    outcome = yield
    rep = outcome.get_result()
    if item.get_closest_marker("eval") is None:
        return
    if rep.when == "call" or (rep.when == "setup" and rep.skipped):
        _append_row({
            "test": item.nodeid,
            "ts": time.time(),
            "status": rep.outcome,  # passed / failed / skipped
            "duration_s": round(rep.duration, 1),
        })
```

- [ ] **Step 4: Implement `agent/evals/report.py`**

```python
#!/usr/bin/env python3
"""Summarize the newest eval run: pass/fail, tokens, wall time per case.

Usage: uv run python evals/report.py [path/to/run.jsonl]
Defaults to results/latest.jsonl (the current run's symlink). Stdlib only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

_RESULTS_DIR = Path(__file__).parent / "results"


def _load(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def summarize(path: Path) -> str:
    rows = _load(path)
    tests: dict[str, dict] = {}
    for row in rows:
        entry = tests.setdefault(row["test"], {
            "status": "?", "duration_s": 0.0, "tokens_in": 0, "tokens_out": 0, "outcomes": [],
        })
        if "status" in row:
            entry["status"] = row["status"]
            entry["duration_s"] = row.get("duration_s", 0.0)
        if "outcome" in row:
            entry["outcomes"].append(row["outcome"])
            entry["tokens_in"] += row.get("tokens_in", 0)
            entry["tokens_out"] += row.get("tokens_out", 0)

    lines = [f"eval run: {path.resolve().name}", ""]
    lines.append(f"{'CASE':<70} {'STATUS':<8} {'WALL(s)':>8} {'TOK IN':>10} {'TOK OUT':>8}  OUTCOMES")
    total_in = total_out = total_wall = 0.0
    counts: dict[str, int] = {}
    for test, e in sorted(tests.items()):
        counts[e["status"]] = counts.get(e["status"], 0) + 1
        total_in += e["tokens_in"]
        total_out += e["tokens_out"]
        total_wall += e["duration_s"]
        name = test.split("::", 1)[-1]
        lines.append(
            f"{name:<70} {e['status']:<8} {e['duration_s']:>8.1f} "
            f"{e['tokens_in']:>10} {e['tokens_out']:>8}  {','.join(e['outcomes']) or '-'}"
        )
    lines.append("")
    summary = ", ".join(f"{n} {status}" for status, n in sorted(counts.items()))
    lines.append(f"{len(tests)} case(s): {summary}")
    lines.append(f"totals: {int(total_in)} tokens in, {int(total_out)} tokens out, {total_wall:.0f}s wall")
    return "\n".join(lines)


def main(argv: list[str]) -> int:
    path = Path(argv[1]) if len(argv) > 1 else _RESULTS_DIR / "latest.jsonl"
    if not path.exists():
        print(f"no results at {path} -- run `make eval` first", file=sys.stderr)
        return 1
    print(summarize(path))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
```

Makefile (add `eval-report` to `.PHONY` too):

```make
## eval-report: summarize the newest eval run (agent/evals/results/latest.jsonl)
eval-report:
	cd agent && uv run python evals/report.py
```

- [ ] **Step 5: Selftests green, then commit the tooling**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -v && uv run ruff check evals && cd .. && make test`
Expected: ALL PASS; `make test` still collects zero evals.

```bash
git add agent/evals/conftest.py agent/evals/report.py agent/evals/test_harness_selftest.py Makefile
git commit -m "feat(evals): per-run results files, status rows, and make eval-report"
```

- [ ] **Step 6: Fix R7 under-recording (separate concern, separate commit)**

Rewrite `test_r7_verdict_consistency_on_clean_pr` in `agent/evals/test_reviewer_evals.py` — record EVERY consistency turn and delete the 4th run that existed only to feed `record_result` (saves one opus turn per run):

```python
def test_r7_verdict_consistency_on_clean_pr(tmp_path: Path, eval_env: Path, record_result) -> None:
    # Same clean PR reviewed 3x on fresh threads: majority must be done. Every
    # run records its own row -- the flip count is the nondeterminism metric
    # to track across runs (see report.py / results/run-*.jsonl).
    repo = _pr_repo(tmp_path, eval_env, _CLEAN_CHANGE)
    outcomes = []
    for i in range(3):
        result = run_reviewer_turn(repo, _REVIEW_ISSUE, thread_id=f"eval-consistency-{i}")
        record_result(result, consistency_run=i)
        outcomes.append(result.outcome)
    passes = sum(1 for o in outcomes if o == Outcome.done)
    assert passes >= 2, f"verdict flipped on identical input: {outcomes}"
```

- [ ] **Step 7: Live sanity (optional but cheap: ~3 opus reviewer turns, ~$1–2)**

Run: `source ~/.secrets && cd agent && uv run pytest evals/test_reviewer_evals.py -k r7 -v && cd .. && make eval-report`
Expected: PASS; report shows 3 metric rows + 1 status row for R7.

```bash
git add agent/evals/test_reviewer_evals.py
git commit -m "fix(evals): record every r7 consistency turn, drop the extra fourth run"
```

---

### Task 2: L2 convergence harness — `run_convergence_loop` + per-issue checkpoint DB

**Design decisions (studied against the dispatcher):**

1. **Thread identity: same raw `thread_id` across every round, both lanes — never a fresh thread per round.** This is exactly what the dispatcher does: `spawnClaim` passes the same `--thread` for a fresh claim and a rework claim (`dispatcher/schedule.go:222`), and a rework re-run is "a fresh `task_text` turn on the same thread" (AGENTS.md) with the feedback injected via `CLIPSE_REVIEW_FEEDBACK` (`dispatcher/spawn.go:55-57`, sourced from `store.LatestReworkFeedback` = the newest `changes_requested` run's **summary**, `internal/store/crud.go:485`). The harness mirrors this by passing `review_feedback=<reviewer.summary>` into the round-N coder turn on the same `thread_id`.
2. **Checkpoint continuity: a shared per-case checkpoint DB, opened per turn.** Production shares one `<checkpoints>/<issue>.db` across every lane/turn of an issue; each worker process opens it fresh (`worker.py:199` `async with AsyncSqliteSaver.from_conn_string(...)`). The harness does the identical thing inside each turn's `asyncio.run` — a fresh event loop per turn, so the saver never crosses loops. This is what makes "same thread" mean something: the round-2 coder's DAC thread resumes its round-1 message history, exactly as in production.
3. **Outer-graph thread namespace: `{thread}::{lane}`, mirroring `worker.py:192`** — the v1 harness's `::outer` suffix was shared by both lanes, which is fine with no checkpointer but corrupts state the moment coder and reviewer share a real one (two structurally different graphs, one thread). Behavior-neutral for all existing checkpointer-less cases.
4. **Branch/PR state carries by construction:** coder pushes to the fixture `origin`; the reviewer reviews the SAME worktree (production also shares one worktree per issue via `ws.Ensure`); the gh shim's `pr.json` persists in the per-test `eval_env` dir across rounds, so round 1's `gh pr create` is visible to every later `pr view`.
5. **Bound and outcomes:** loop caps at `max_rounds` (default 3, the case-level rework budget analogue); `done` stops the loop with `rounds_to_done = len(rounds)`; a coder non-`needs_review` or reviewer non-`changes_requested`/`done` outcome stops it (the kernel would park) with `rounds_to_done = None`. Turn functions are injectable so the loop's plumbing is provable with zero LLM cost.

**Files:**
- Modify: `agent/evals/harness.py`
- Test: `agent/evals/test_harness_selftest.py`

- [ ] **Step 1: Write the failing selftests (no LLM)**

Append to `agent/evals/test_harness_selftest.py`:

```python
from clipse_agent.contract import Outcome
from harness import run_convergence_loop


def test_convergence_loop_threads_feedback_and_stops_on_done(tmp_path: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    coder_calls: list[str] = []

    def fake_coder(r, issue_text, *, review_feedback="", thread_id="", checkpoint_db=None, **_):
        coder_calls.append(review_feedback)
        return _wr(Outcome.needs_review)

    # _wr defaults summary="s" -- the loop must thread that exact string into
    # the round-2 coder call (what LatestReworkFeedback would carry).
    reviewer_results = iter([
        _wr(Outcome.changes_requested), _wr(Outcome.done),
    ])

    def fake_reviewer(r, issue_text, *, thread_id="", checkpoint_db=None, **_):
        return next(reviewer_results)

    out = run_convergence_loop(
        repo, "task", coder_turn=fake_coder, reviewer_turn=fake_reviewer,
        thread_id="t", checkpoint_db=None,
    )
    assert out.rounds_to_done == 2
    assert len(out.rounds) == 2
    assert coder_calls[0] == ""            # round 1: fresh task, no feedback
    assert coder_calls[1] == "s"           # round 2: the reviewer's summary verbatim
    assert out.tokens_in == 4 * 10 and out.tokens_out == 4 * 2


def test_convergence_loop_bounds_at_max_rounds(tmp_path: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    out = run_convergence_loop(
        repo, "task",
        coder_turn=lambda *a, **k: _wr(Outcome.needs_review),
        reviewer_turn=lambda *a, **k: _wr(Outcome.changes_requested),
        max_rounds=3, checkpoint_db=None,
    )
    assert out.rounds_to_done is None and len(out.rounds) == 3


def test_convergence_loop_stops_when_coder_blocks(tmp_path: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    out = run_convergence_loop(
        repo, "task",
        coder_turn=lambda *a, **k: _wr(Outcome.blocked),
        reviewer_turn=lambda *a, **k: (_ for _ in ()).throw(AssertionError("reviewer must not run")),
        checkpoint_db=None,
    )
    assert out.rounds_to_done is None
    assert len(out.rounds) == 1 and out.rounds[0].reviewer is None
```

(`_wr` from Task 1; adjust it to accept a `summary="s"` default so the feedback assertion above works — `_wr(Outcome.changes_requested)` carries summary `"s"`. Blocked results need `block_kind`: extend `_wr` with `block_kind=BlockKind.needs_input if outcome is Outcome.blocked else None`.)

- [ ] **Step 2: Verify failure**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -k convergence -v`
Expected: FAIL (`ImportError: run_convergence_loop`).

- [ ] **Step 3: Implement in `agent/evals/harness.py`**

Add imports and the checkpointer-aware runner; change the outer thread namespace:

```python
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

from clipse_agent.contract import Outcome, WorkerResult
```

```python
def _lane_config(thread_id: str, lane: str) -> dict:
    # Mirrors worker.py's outer-graph namespace (f"{args.thread}::{lane}"):
    # the coder and reviewer wrapping graphs are structurally different, so
    # once they share a physical checkpointer they need distinct threads.
    return {"configurable": {"thread_id": f"{thread_id}::{lane}"}}


async def _ainvoke(build_graph, state: dict, config: dict, checkpoint_db: Path | None, **extra):
    """One graph turn, opened against the shared per-case checkpoint DB the
    way worker.py does per process: a fresh AsyncSqliteSaver per turn (each
    asyncio.run owns its own event loop, so the saver never crosses loops)."""
    if checkpoint_db is None:
        graph = build_graph(checkpointer=None, **extra)
        final = await graph.ainvoke(state, config)
        return final["result"]
    checkpoint_db.parent.mkdir(parents=True, exist_ok=True)
    async with AsyncSqliteSaver.from_conn_string(str(checkpoint_db)) as saver:
        graph = build_graph(checkpointer=saver, **extra)
        final = await graph.ainvoke(state, config)
        return final["result"]
```

Rewrite the two turn runners on top of it (public signatures grow one kw-only arg; existing callers unchanged):

```python
def run_coder_turn(
    repo: FixtureRepo,
    issue_text: str,
    *,
    review_feedback: str = "",
    max_tokens: int = 400_000,
    thread_id: str = "eval-thread",
    checkpoint_db: Path | None = None,
) -> WorkerResult:
    state = _input_state(repo, issue_text, max_tokens=max_tokens, thread_id=thread_id)
    if review_feedback:
        state["review_feedback"] = review_feedback
    return asyncio.run(_ainvoke(
        build_coder_graph, state, _lane_config(thread_id, "coder"), checkpoint_db,
        profile=get_coder_profile(EVAL_MODEL), docs_profile=get_coder_docs_profile(EVAL_MODEL),
    ))


def run_reviewer_turn(
    repo: FixtureRepo,
    issue_text: str,
    *,
    max_tokens: int = 400_000,
    thread_id: str = "eval-thread",
    checkpoint_db: Path | None = None,
) -> WorkerResult:
    state = _input_state(repo, issue_text, max_tokens=max_tokens, thread_id=thread_id)
    return asyncio.run(_ainvoke(
        build_reviewer_graph, state, _lane_config(thread_id, "reviewer"), checkpoint_db,
        profile=get_reviewer_profile(EVAL_MODEL),
    ))
```

Add the loop:

```python
@dataclass(frozen=True)
class RoundResult:
    coder: WorkerResult
    reviewer: WorkerResult | None  # None when the coder never reached review
    coder_head: str  # branch tip after the coder turn (rework diff-change tracking)


@dataclass(frozen=True)
class ConvergenceResult:
    rounds: list[RoundResult]
    rounds_to_done: int | None  # 1-based round whose review passed; None = never converged

    @property
    def tokens_in(self) -> int:
        return sum(r.coder.tokens.in_ + (r.reviewer.tokens.in_ if r.reviewer else 0) for r in self.rounds)

    @property
    def tokens_out(self) -> int:
        return sum(r.coder.tokens.out + (r.reviewer.tokens.out if r.reviewer else 0) for r in self.rounds)

    @property
    def round_outcomes(self) -> list[list[str]]:
        return [
            [r.coder.outcome.value, r.reviewer.outcome.value if r.reviewer else None]
            for r in self.rounds
        ]


def run_convergence_loop(
    repo: FixtureRepo,
    issue_text: str,
    *,
    max_rounds: int = 3,
    thread_id: str = "eval-thread",
    checkpoint_db: Path | None = None,
    coder_turn=run_coder_turn,
    reviewer_turn=run_reviewer_turn,
) -> ConvergenceResult:
    """Drive the FULL coder -> reviewer -> rework loop in-process, mirroring
    the dispatcher's rework mechanics (no dispatcher, no Linear):

    - round 1: fresh coder turn (no feedback), then a reviewer turn;
    - round N>1: fresh task_text on the SAME thread_id, with the previous
      reviewer's changes_requested `summary` as review_feedback -- byte-for-
      byte what store.LatestReworkFeedback injects via CLIPSE_REVIEW_FEEDBACK;
    - both lanes share one worktree, one gh-shim PR state, and (when
      checkpoint_db is set) one per-issue checkpoint DB, like production.

    Stops early on `done` (rounds_to_done = that round, 1-based) or on any
    outcome the kernel would park (coder blocked, reviewer blocked); runs at
    most max_rounds otherwise. `coder_turn`/`reviewer_turn` are injectable so
    the loop's plumbing is testable with zero LLM cost.
    """
    rounds: list[RoundResult] = []
    feedback = ""
    for _ in range(max_rounds):
        coder = coder_turn(
            repo, issue_text, review_feedback=feedback,
            thread_id=thread_id, checkpoint_db=checkpoint_db,
        )
        head = git_out(repo.worktree, "rev-parse", "HEAD")
        if coder.outcome != Outcome.needs_review:
            rounds.append(RoundResult(coder=coder, reviewer=None, coder_head=head))
            break
        reviewer = reviewer_turn(repo, issue_text, thread_id=thread_id, checkpoint_db=checkpoint_db)
        rounds.append(RoundResult(coder=coder, reviewer=reviewer, coder_head=head))
        if reviewer.outcome == Outcome.done:
            return ConvergenceResult(rounds=rounds, rounds_to_done=len(rounds))
        if reviewer.outcome != Outcome.changes_requested:
            break  # reviewer blocked -> the kernel would park, not re-run the coder
        feedback = reviewer.summary  # what LatestReworkFeedback would carry
    return ConvergenceResult(rounds=rounds, rounds_to_done=None)
```

- [ ] **Step 4: All selftests + existing suites green**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -v && uv run pytest && uv run ruff check evals`
Expected: ALL PASS (the `::outer`→`::coder`/`::reviewer` rename is invisible to checkpointer-less cases).

- [ ] **Step 5: Commit**

```bash
git add agent/evals/harness.py agent/evals/test_harness_selftest.py
git commit -m "feat(evals): convergence-loop harness with per-issue checkpoint db"
```

---

### Task 3: L2 cases — seeded-bug convergence (L2a) and clean-task convergence (L2b)

**Case design.** L2a plants the defect in the PR **branch** (a pre-seeded buggy commit, like `_rework_repo`), not in the coder's own round-1 output — planting it in the coder's output would be praying for a model mistake. The round-1 coder task is deliberately adjacent (add a weak test that passes against the buggy impl), so the round-1 review of the full `base...HEAD` diff should catch the off-by-one and request changes; round 2's coder gets only the reviewer's summary and must fix it. If a conscientious round-1 coder fixes the bug unprompted, the case still converges honestly at `rounds_to_done=1` — the assertions never demand `== 2`.

**R6 actionability is an assertion inside L2a, not a separate judge:** whenever round 1 requested changes, the round-2 coder must have (a) changed the diff (`coder_head` moved — the CLI-15 dead-loop guard at loop level) and (b) actually fixed the planted defect (deterministic `python3 -c` check). Passing both means the reviewer's summary alone carried enough signal; the recorded `feedback_addressed` field tracks it across runs.

**Files:**
- Create: `agent/evals/test_convergence_evals.py`

- [ ] **Step 1: Write the cases**

```python
"""L2 convergence evals: the full coder -> reviewer -> rework loop in-process.
The deliverable metric is cost-per-approved-PR: rounds_to_done + total tokens
per case, recorded to results/run-*.jsonl. Assertions are honest bounds
(<= max_rounds), never a hard round count that flakes on a good round 1."""
from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from harness import (
    commit_on_branch,
    make_fixture_repo,
    requires_anthropic,
    run_convergence_loop,
)

pytestmark = [pytest.mark.eval, requires_anthropic]

# Expected cost: L2a ~2 coder turns (sonnet, each incl. the docs sub-step)
# + ~2 reviewer turns (opus) -> roughly $3-6 and 10-20 min. L2b about half.

_CALC_BUGGY = (
    "def total(xs):\n"
    "    result = 0\n"
    "    for i in range(len(xs) - 1):\n"
    "        result += xs[i]\n"
    "    return result\n"
)
_CALC_FIXED_CHECK = ["python3", "-c", "import calc; assert calc.total([1, 2, 3]) == 6"]


def test_l2a_seeded_defect_review_drives_convergence(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(
        tmp_path,
        files={"README.md": "# calc\n`total(xs)` sums a list.\n"},
    )
    # The PR-under-review already carries the planted off-by-one; the coder's
    # own task is adjacent (a weak test that PASSES against the buggy impl),
    # so round 1's review should be the thing that surfaces the defect.
    commit_on_branch(repo, {"calc.py": _CALC_BUGGY}, "EVAL-1: implement total")

    out = run_convergence_loop(
        repo,
        "EVAL-1: finish the total() PR.\n\n"
        "The branch already implements total(xs) in calc.py. Add test_calc.py "
        "with a test asserting total([]) == 0, and open the PR for review.",
        max_rounds=3,
        thread_id="eval-l2a",
        checkpoint_db=tmp_path / "checkpoints" / "EVAL-1.db",
    )

    last = out.rounds[-1]
    feedback_addressed = (
        len(out.rounds) < 2 or out.rounds[1].coder_head != out.rounds[0].coder_head
    )
    record_result(
        last.reviewer or last.coder,
        rounds_to_done=out.rounds_to_done,
        round_outcomes=out.round_outcomes,
        loop_tokens_in=out.tokens_in,
        loop_tokens_out=out.tokens_out,
        feedback_addressed=feedback_addressed,
    )

    assert out.rounds_to_done is not None, f"never converged: {out.round_outcomes}"
    assert out.rounds_to_done <= 3
    # The planted defect is gone by convergence, whichever round fixed it.
    check = subprocess.run(_CALC_FIXED_CHECK, cwd=repo.worktree, capture_output=True, text=True)
    assert check.returncode == 0, check.stderr
    # R6 actionability: a rework round that shipped a byte-identical diff is
    # the CLI-15 dead loop surfacing at loop level -- the reviewer's summary
    # did not carry enough signal (AGENTS.md open follow-up made measurable).
    assert feedback_addressed, "round-2 coder did not change the diff after changes_requested"
    assert (eval_env / "pr.json").exists(), "converged without ever opening a PR"


def test_l2b_clean_task_converges_first_round(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# app\n"})
    out = run_convergence_loop(
        repo,
        "EVAL-1: add an AUTHORS file listing 'clipse evals'.\n\n"
        "Create AUTHORS at the repo root with the single line 'clipse evals'.",
        max_rounds=3,
        thread_id="eval-l2b",
        checkpoint_db=tmp_path / "checkpoints" / "EVAL-1.db",
    )

    last = out.rounds[-1]
    record_result(
        last.reviewer or last.coder,
        rounds_to_done=out.rounds_to_done,
        round_outcomes=out.round_outcomes,
        loop_tokens_in=out.tokens_in,
        loop_tokens_out=out.tokens_out,
    )

    assert out.rounds_to_done is not None, f"never converged: {out.round_outcomes}"
    # Expect 1; tolerate one extra round (an over-strict blocking finding on a
    # trivial diff is reviewer-calibration signal -- visible in the recorded
    # metric -- not a harness failure). >2 fails: that is a broken loop.
    assert out.rounds_to_done <= 2, f"clean trivial PR burned {out.rounds_to_done} review rounds"
```

- [ ] **Step 2: Collection + skip behavior without the key**

Run: `cd agent && env -u ANTHROPIC_API_KEY uv run pytest evals/test_convergence_evals.py -v`
Expected: 2 skipped (skip reason names `ANTHROPIC_API_KEY`), 0 errors. `uv run ruff check evals` clean.

- [ ] **Step 3: Live run**

Run: `source ~/.secrets && cd agent && uv run pytest evals/test_convergence_evals.py -v && cd .. && make eval-report`
Expected: both PASS; report shows `rounds_to_done` in the recorded rows (inspect `agent/evals/results/latest.jsonl` for the L2a `round_outcomes`). If L2a converges at round 1 (coder fixed the bug unprompted), note it — the case remains green by design; if it exhausts 3 rounds on vague feedback, that is the known AGENTS.md rework-feedback gap surfacing as a red eval (see Self-Review).

- [ ] **Step 4: Commit**

```bash
git add agent/evals/test_convergence_evals.py
git commit -m "feat(evals): l2 coder-reviewer convergence cases with rounds-to-done metric"
```

---

### Task 4: D3 docs-accuracy judge — minimal `judge.py` + one case

**Client choice (checked, not guessed):** `agent/uv.lock` carries `anthropic` and `langchain-anthropic` only as *transitive* deps of `deepagents-code`; importing them directly would repeat the exact transitive-import bug v1 fixed for `langgraph-checkpoint-sqlite`. `deepagents_code.config.create_model` IS a direct dep's public factory, already imported by `dac.py`, and returns a wrapper whose `.model` is a LangChain `BaseChatModel` (`dac.py:179`) — `model.invoke([...])` gives the judge a one-shot call with zero new dependencies. Judge model: `anthropic:claude-haiku-4-5` (the smallest current anthropic model), **pinned regardless of `CLIPSE_EVAL_MODEL`** so judge verdicts stay comparable across the codex matrix; hence its own skip guard on `ANTHROPIC_API_KEY`.

**Scope guard:** the judge is used by exactly ONE case (D3). The `langsmith[pytest]` upgrade path (judge-as-feedback on traces) is deferred — noted in the README.

**Files:**
- Create: `agent/evals/judge.py`
- Modify: `agent/evals/test_docs_evals.py` (add D3), `agent/evals/harness.py` (skip marker)
- Test: `agent/evals/test_harness_selftest.py` (parser, no LLM)

- [ ] **Step 1: Failing parser tests (no LLM)**

Append to `agent/evals/test_harness_selftest.py`:

```python
from judge import _parse_verdict


def test_judge_parser_accepts_strict_contract() -> None:
    assert _parse_verdict('{"pass": true, "reason": "ok"}') is True
    assert _parse_verdict('{"pass": false, "reason": "no"}') is False


def test_judge_parser_tolerates_surrounding_prose_and_fences() -> None:
    assert _parse_verdict('Sure!\n```json\n{"pass": true, "reason": "r"}\n```') is True


def test_judge_parser_rejects_garbage() -> None:
    assert _parse_verdict("PASS") is None
    assert _parse_verdict('{"pass": "yes"}') is None
    assert _parse_verdict("") is None
```

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -k judge_parser -v` — expected FAIL (no module).

- [ ] **Step 2: Implement `agent/evals/judge.py`**

```python
"""Minimal LLM judge for evals needing a semantic yes/no (v2: ONE case, D3).

Built on deepagents_code.config.create_model -- the only model factory
reachable through a DIRECT dependency (`anthropic`/`langchain-anthropic` are
transitive-only in uv.lock, and importing a transitive is a bug in this repo).
The judge model is pinned to the smallest current anthropic model and does
NOT follow CLIPSE_EVAL_MODEL: verdicts must stay comparable across the model
matrix, so a codex run still judges with haiku (and skips D3 without
ANTHROPIC_API_KEY -- see harness.requires_judge).

Upgrade path (deferred): langsmith[pytest] judge-as-feedback once judge usage
grows past one case.
"""
from __future__ import annotations

import json
from typing import Any

from deepagents_code.config import create_model

JUDGE_MODEL = "anthropic:claude-haiku-4-5"

_PROMPT = """You are a strict evaluator. Judge the evidence against the rubric.

## Rubric
{rubric}

## Evidence
{evidence}

Answer with ONLY this JSON object, nothing else:
{{"pass": true or false, "reason": "<one short sentence>"}}"""


class JudgeError(RuntimeError):
    """The judge produced no parseable verdict, even after one retry."""


def _message_text(message: Any) -> str:
    """Extract the text of a LangChain AIMessage: str content, or the text
    blocks of a structured content list (same shape dac.py consumes)."""
    content = message.content
    if isinstance(content, str):
        return content
    return "".join(
        block.get("text", "")
        for block in content
        if isinstance(block, dict) and block.get("type") == "text"
    )


def _parse_verdict(text: str) -> bool | None:
    """Return the verdict, or None when the strict contract is absent."""
    start, end = text.find("{"), text.rfind("}")
    if start == -1 or end <= start:
        return None
    try:
        payload = json.loads(text[start : end + 1])
    except json.JSONDecodeError:
        return None
    verdict = payload.get("pass") if isinstance(payload, dict) else None
    return verdict if isinstance(verdict, bool) else None


def judge(rubric: str, evidence: str) -> bool:
    """One yes/no judgment. Raises JudgeError on an unparseable reply after
    one retry -- a broken judge must fail the case loudly, never pass it."""
    model = create_model(JUDGE_MODEL).model
    prompt = _PROMPT.format(rubric=rubric, evidence=evidence)
    messages = [{"role": "user", "content": prompt}]
    for _ in range(2):  # initial attempt + one retry
        verdict = _parse_verdict(_message_text(model.invoke(messages)))
        if verdict is not None:
            return verdict
        messages = [{"role": "user", "content": prompt + "\n\nReply with ONLY the JSON object."}]
    raise JudgeError('judge returned no parseable {"pass": bool} verdict after a retry')
```

Add to `agent/evals/harness.py` (next to `requires_anthropic`):

```python
# The docs-accuracy judge is pinned to an anthropic model regardless of
# CLIPSE_EVAL_MODEL, so it needs the key even on a codex-matrix run.
requires_judge = pytest.mark.skipif(
    not os.environ.get("ANTHROPIC_API_KEY"),
    reason="LLM judge is pinned to anthropic:claude-haiku-4-5; ANTHROPIC_API_KEY required",
)
```

- [ ] **Step 3: Add D3 to `agent/evals/test_docs_evals.py`**

```python
from harness import git_out, requires_judge
from judge import judge

_D3_RUBRIC = (
    "The README's documentation of the newly added CLI flag accurately "
    "describes what the code diff implements: the flag name matches, the "
    "described behavior matches the argparse definition (a boolean "
    "store_true flag), and the README does not document flags, options, or "
    "behavior that the diff does not add."
)


@requires_judge
def test_d3_docs_edit_accurately_describes_the_change(tmp_path: Path, eval_env: Path, record_result) -> None:
    # Expected cost: one D1-style coder turn (sonnet) + one haiku judge call
    # (well under a cent for the judge).
    repo = make_fixture_repo(tmp_path, files=_CLI_FILES)
    result = run_coder_turn(
        repo,
        "EVAL-1: add a --shout flag to cli.py.\n\n"
        "Add a boolean `--shout` flag (store_true) to the parser in cli.py.",
    )
    # Deterministic gates first (same shape as D1): a judge fail must mean
    # "docs are inaccurate", never "the task itself failed".
    assert result.outcome == Outcome.needs_review
    readme = (repo.worktree / "README.md").read_text()
    assert "--shout" in readme

    code_diff = git_out(
        repo.worktree, "diff", f"origin/{repo.base_branch}...HEAD",
        "--", ".", ":(exclude)README.md",
    )
    verdict = judge(
        rubric=_D3_RUBRIC,
        evidence=f"## Code diff (docs excluded)\n{code_diff}\n\n## README after the change\n{readme}",
    )
    record_result(result, judge_pass=verdict)
    assert verdict, "README edit does not accurately describe the code change"
```

- [ ] **Step 4: Verify skip/collect/lint, then live**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -k judge_parser -v && env -u ANTHROPIC_API_KEY uv run pytest evals/test_docs_evals.py -k d3 -v && uv run ruff check evals`
Expected: parser tests PASS; D3 skips with the judge-specific reason.

Live: `source ~/.secrets && cd agent && uv run pytest evals/test_docs_evals.py -k d3 -v`
Expected: PASS; `judge_pass: true` in the run file.

- [ ] **Step 5: Commit**

```bash
git add agent/evals/judge.py agent/evals/harness.py agent/evals/test_docs_evals.py agent/evals/test_harness_selftest.py
git commit -m "feat(evals): haiku docs-accuracy judge + d3 case"
```

---

### Task 5: gh shim minors — unhandled-subcommand warning + `pr view` branch-arg note

Unhandled subcommands keep exiting 0 (a DAC agent probing `gh repo view` must not look like a hard platform failure and trigger retry loops — the v1 posture), but now say so on stderr, so a case author reading a worker's stderr log learns the shim answered nothing.

**Files:**
- Modify: `agent/evals/gh_shim/gh`
- Test: `agent/evals/test_harness_selftest.py`

- [ ] **Step 1: Failing selftest**

```python
def test_gh_shim_warns_on_unhandled_subcommand(tmp_path: Path, eval_env: Path) -> None:
    proc = subprocess.run(
        ["gh", "repo", "view", "--json", "name"],
        cwd=tmp_path, capture_output=True, text=True, env=os.environ.copy(),
    )
    assert proc.returncode == 0  # still success: never look like a hard failure
    assert "unhandled" in proc.stderr
    calls = [json.loads(line) for line in (eval_env / "calls.jsonl").read_text().splitlines()]
    assert calls[-1]["argv"][:2] == ["repo", "view"]
```

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -k unhandled -v` — expected FAIL (empty stderr).

- [ ] **Step 2: Implement in `agent/evals/gh_shim/gh`**

In `main`, annotate `pr view` and replace the bare final `return 0`:

```python
    if argv[:2] == ["pr", "view"]:
        # NOTE: the branch argument is ignored -- the shim keeps exactly ONE
        # pr.json per test dir, which is fine while every case drives a single
        # PR. Revisit if a case ever needs two concurrent branches.
        if pr_file.exists():
            ...
```

```python
    # Anything not modeled above: still exit 0 (an agent probing gh must not
    # see a hard failure and retry-loop), but say so -- silence here means an
    # eval quietly asserted against a shim that answered nothing.
    sys.stderr.write(f"gh shim: unhandled subcommand {' '.join(argv[:2])!r} (no-op, exit 0)\n")
    return 0
```

- [ ] **Step 3: Selftests green + C11 stays green (requirement)**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -v`
Then the C11 requirement (live, one sonnet coder turn ~$0.50): `source ~/.secrets && uv run pytest evals/test_coder_evals.py -k c11 -v`
Expected: ALL PASS — the injection canary is unaffected by shim stderr.

- [ ] **Step 4: Commit**

```bash
git add agent/evals/gh_shim/gh agent/evals/test_harness_selftest.py
git commit -m "feat(evals): gh shim warns on unhandled subcommands"
```

---

### Task 6: Narrow the reviewer's post_comments no-PR grace (worker src + unit test)

Today ANY `gh pr view` failure takes the grace path (`reviewer.py:512-514`) — an auth failure, rate limit, or network outage silently drops every inline finding AND the summary, and the run still emits a clean `changes_requested`/`done`. Narrow the grace to the one legitimate case (the coder's honest `pr_url=""` no-op path — `gh` prints `no pull requests found` on stderr, which the shim mirrors); anything else raises `ReviewerGraphError` → the worker's catch-all emits `blocked/transient` → the kernel's bounded retry gets a shot instead of findings vanishing. One production-code file, one test file, one commit.

**Files:**
- Modify: `agent/src/clipse_agent/graphs/reviewer.py` (`make_post_comments`)
- Test: `agent/tests/test_reviewer_graph.py`

- [ ] **Step 1: Failing unit test**

Append next to `test_post_comments_no_pr_is_graceful` (which already stubs stderr `"no pull requests found"` and must stay green):

```python
def test_post_comments_pr_view_transient_failure_raises() -> None:
    # A gh failure that is NOT the no-PR case must raise -> blocked/transient
    # -> kernel retry, instead of silently dropping every finding.
    runner = FakeRunner(
        rules=[
            (_starts_with("gh", "pr", "view"),
             coder.CommandResult(1, "", "connect: network is unreachable")),
        ]
    )
    node = reviewer.make_post_comments(runner)
    with pytest.raises(reviewer.ReviewerGraphError, match="gh pr view"):
        node({"branch": "clipse/EVAL-1", "cwd": "/tmp", "review_comments": []})
```

Run: `cd agent && uv run pytest tests/test_reviewer_graph.py -k pr_view -v` — expected FAIL (grace path returns instead of raising).

- [ ] **Step 2: Implement**

In `make_post_comments._node`, replace the blanket grace:

```python
        view = _run(run_command, ["gh", "pr", "view", branch, "--json", "number,headRefOid,url"], cwd, check=False)
        if view.returncode != 0:
            # Narrow grace: only the known no-PR signal (the coder's honest
            # pr_url="" no-op path -- gh prints "no pull requests found").
            # Any OTHER failure (auth, network, rate limit) raises so the
            # kernel's transient retry machinery gets a shot, instead of the
            # findings silently vanishing while the run reports success.
            if "no pull requests found" in view.stderr.lower():
                return {"pr_url": None, "comments_posted": 0, "comments_failed": 0, "failed_comments": []}
            raise ReviewerGraphError(
                f"post_comments: gh pr view {branch} failed (exit {view.returncode}): {view.stderr.strip()}"
            )
```

Update the function docstring's `check=False` paragraph to say the pr-view grace is now scoped to the no-PR stderr signal (the per-comment `gh api` and summary `gh pr comment` best-effort behavior is unchanged).

- [ ] **Step 3: Suites green**

Run: `cd agent && uv run pytest tests/test_reviewer_graph.py -v && uv run pytest && cd .. && make test && make lint`
Expected: ALL PASS (the existing no-PR grace test keeps passing — its stub already used the real stderr string).

- [ ] **Step 4: Commit**

```bash
git add agent/src/clipse_agent/graphs/reviewer.py agent/tests/test_reviewer_graph.py
git commit -m "fix(reviewer): narrow post_comments no-pr grace to the real no-pr signal"
```

---

### Task 7: R5 placement validity — smoke-side, report-only

A small stdlib-Python script (shells out to `gh api`; `scripts/smoke/` is bash-first, but hunk-header parsing in bash/jq is not worth it) plus a non-fatal hook in `smoke.sh verify()`. **Clearly optional: needs live GitHub + a completed smoke run; it never affects the smoke PASS/FAIL.** Two numbers per PR: the in-hunk rate of comments that DID post, and the count the reviewer itself reported as unplaceable (parsed from `_changes_summary`'s deterministic `"N finding(s) could not be attached"` line) — together they approximate attempted-vs-placed.

**Files:**
- Create: `scripts/smoke/check-placement.py`
- Modify: `scripts/smoke/smoke.sh` (`verify()`)

- [ ] **Step 1: Write the script (with `--self-test` for the hunk parser)**

```python
#!/usr/bin/env python3
"""R5: reviewer inline-comment placement validity against a REAL GitHub repo.

Report-only, optional -- requires `gh` auth and a completed smoke run. For
each smoke PR it compares posted inline review comments (gh api) against the
PR diff hunks, and also counts the findings the reviewer itself reported as
unplaceable in its summary comment ("N finding(s) could not be attached").

Usage:
  check-placement.py --repo owner/name [--pr N ...] [--self-test]

Exit code is always 0 unless gh itself fails -- placement rate is a metric,
not a gate.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
UNATTACHED_RE = re.compile(r"(\d+) finding\(s\) could not be attached")
SMOKE_TITLE_RE = re.compile(r"^[A-Z]+-\d+[: ]")


def gh_json(args: list[str]):
    proc = subprocess.run(["gh", *args], capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"gh {' '.join(args)} failed: {proc.stderr.strip()}")
    return json.loads(proc.stdout or "[]")


def hunk_lines(patch: str) -> set[int]:
    """RIGHT-side line numbers a patch's hunks cover (added + context lines)
    -- the lines GitHub accepts an inline review comment on."""
    covered: set[int] = set()
    current: int | None = None
    for raw in (patch or "").splitlines():
        match = HUNK_RE.match(raw)
        if match:
            current = int(match.group(1))
            continue
        if current is None or raw.startswith("-"):
            continue
        if raw.startswith(("+", " ")):
            covered.add(current)
            current += 1
    return covered


def check_pr(repo: str, number: int) -> tuple[int, int, int]:
    files = gh_json(["api", f"repos/{repo}/pulls/{number}/files", "--paginate"])
    hunks = {f["filename"]: hunk_lines(f.get("patch", "")) for f in files}
    comments = gh_json(["api", f"repos/{repo}/pulls/{number}/comments", "--paginate"])
    posted = len(comments)
    placed = sum(
        1 for c in comments
        if (c.get("line") or c.get("original_line")) in hunks.get(c.get("path", ""), set())
    )
    issue_comments = gh_json(["api", f"repos/{repo}/issues/{number}/comments", "--paginate"])
    unattached = sum(
        int(m.group(1))
        for c in issue_comments
        if (m := UNATTACHED_RE.search(c.get("body") or ""))
    )
    return posted, placed, unattached


def self_test() -> int:
    patch = "@@ -1,3 +1,4 @@\n context\n+added\n context\n-removed\n context\n@@ -10 +12,2 @@\n+a\n+b\n"
    lines = hunk_lines(patch)
    assert lines == {1, 2, 3, 4, 12, 13}, lines
    assert hunk_lines("") == set()
    assert UNATTACHED_RE.search("2 finding(s) could not be attached to a diff line").group(1) == "2"
    print("self-test ok")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo")
    parser.add_argument("--pr", type=int, action="append", default=[])
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.repo:
        parser.error("--repo is required (unless --self-test)")

    numbers = args.pr or [
        pr["number"]
        for pr in gh_json(["pr", "list", "--repo", args.repo, "--state", "all",
                           "--limit", "200", "--json", "number,title"])
        if SMOKE_TITLE_RE.match(pr["title"])
    ]
    total_posted = total_placed = total_unattached = 0
    print(f"{'PR':>5} {'POSTED':>7} {'PLACED':>7} {'UNATTACHED':>11}")
    for number in sorted(numbers):
        posted, placed, unattached = check_pr(args.repo, number)
        total_posted += posted
        total_placed += placed
        total_unattached += unattached
        print(f"{number:>5} {posted:>7} {placed:>7} {unattached:>11}")
    attempted = total_posted + total_unattached
    rate = (total_placed / attempted * 100) if attempted else 100.0
    print(f"\nplacement rate: {total_placed}/{attempted} attempted findings placed inline ({rate:.0f}%)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

`chmod +x scripts/smoke/check-placement.py`.

- [ ] **Step 2: Verify the parser**

Run: `python3 scripts/smoke/check-placement.py --self-test`
Expected: `self-test ok`.

- [ ] **Step 3: Hook into `smoke.sh verify()` (non-fatal, after the fails accounting)**

In `verify()`, immediately before the final `if [[ "$fails" -eq 0 ]]` block:

```bash
  # R5 (optional, report-only): reviewer inline-comment placement validity.
  # Never affects the smoke verdict; requires gh auth against $TARGET_REPO.
  if command -v python3 >/dev/null 2>&1; then
    info "verify: inline-comment placement (report-only)"
    python3 "$(dirname "${BASH_SOURCE[0]}")/check-placement.py" --repo "$TARGET_REPO" \
      || warn "placement check failed (non-fatal)"
  fi
```

- [ ] **Step 4: Syntax check + optional live check**

Run: `bash -n scripts/smoke/smoke.sh`
Optional (live GitHub, after any completed smoke run): `python3 scripts/smoke/check-placement.py --repo <TARGET_REPO>` — expect a table + a rate line.

- [ ] **Step 5: Commit**

```bash
git add scripts/smoke/check-placement.py scripts/smoke/smoke.sh
git commit -m "feat(smoke): reviewer inline-comment placement check (report-only)"
```

---

### Task 8: Nightly / model-matrix runbook (docs-only)

**Files:**
- Modify: `agent/evals/README.md`

- [ ] **Step 1: Update the results paragraph and replace the "Deferred (v2)" section**

Update the "Per-case metrics" paragraph:

```markdown
Per-case metrics append to `results/run-<utc-ts>.jsonl` (one file per pytest
session, gitignored); `results/latest.jsonl` is a symlink to the newest run.
A status row per case (pass/fail/skip + wall time) is appended by a conftest
hook. Summarize the newest run with `make eval-report` (or
`uv run python evals/report.py path/to/run.jsonl` for an older one).
```

Replace `## Deferred (v2)` with:

```markdown
## Model matrix & cadence

`CLIPSE_EVAL_MODEL` overrides the lane model for the whole run:

    CLIPSE_EVAL_MODEL=openai_codex:gpt-5.1-codex make eval

Codex prerequisite (once per host, as the dispatcher's OS user): interactive
ChatGPT sign-in via `uv --project agent run dcode` -> `/auth` -> `openai_codex`;
the token lands at `~/.deepagents/.state/chatgpt-auth.json` and auto-refreshes
(see AGENTS.md). The anthropic skip guard does not apply to lane turns on a
codex run -- but D3's docs-accuracy judge is PINNED to
`anthropic:claude-haiku-4-5` so verdicts stay comparable across the matrix:
without `ANTHROPIC_API_KEY` in the env, D3 skips and everything else runs.

Recommended cadence (no automation infra -- deliberate):

- **Nightly local:** a cron/launchd line on the dev machine, e.g.
  `cd ~/Code/clipse && source ~/.secrets && make eval && make eval-report`
  once for the default lane models, optionally a second run with
  `CLIPSE_EVAL_MODEL` for the codex matrix. Runs are cheap enough to eyeball
  the next morning via `results/run-*.jsonl`.
- **Manual pre-release:** before tagging or bumping a lane model / the DAC
  pin, run the full suite on both the default models and each configured
  matrix model, and compare `rounds_to_done` / token totals against the last
  few run files.

Cost: full default-model run is on the order of $10-20 (the L2 convergence
cases dominate -- each is 2-6 live turns with reviewer rounds on opus).
Filter with `-k` when iterating on one case.

## Deferred

R5 as a pytest eval (placement is smoke-side: `scripts/smoke/check-placement.py`),
`langsmith[pytest]` judge feedback, nightly automation infra,
failure-archive->eval-case pipeline.
```

- [ ] **Step 2: Verify docs claims**

Run: `make eval-report` (works against the latest run) and `env -u ANTHROPIC_API_KEY uv --project agent run pytest evals -k d3 --collect-only -q` (D3 collects; the skip guard text matches the README claim).

- [ ] **Step 3: Commit**

```bash
git add agent/evals/README.md
git commit -m "docs(evals): model-matrix + cadence runbook, per-run results docs"
```

---

## Self-Review

### Design risks

1. **L2a's premise is behavioral, not guaranteed.** The case engineers a round-1 `changes_requested` (defect planted in the PR branch; the coder's task is adjacent), but a conscientious round-1 coder may fix the bug unprompted → `rounds_to_done=1` and the rework path goes unexercised that run. The assertions stay green by design (honest bounds); the recorded `round_outcomes` tracks how often the intended 2-round shape actually occurs. If it degrades to 1 round most runs, strengthen the setup (e.g. instruct the coder "only add the test; do not modify calc.py") — noted as a tuning knob, not done pre-emptively.
2. **L2a can go red for a REAL reason.** AGENTS.md documents that rework feedback carries the reviewer's rollup summary, which "can go vague... so convergence can hinge on the coder recovering specifics". If that gap bites, L2a exhausts 3 rounds and fails. That is intended pressure — the case exists to measure exactly this — but implementers should triage a red L2a as "the known summary-vagueness gap" (fix: the AGENTS.md follow-up — thread inline findings or force the reviewer to restate them), not by loosening the eval.
3. **Checkpointer sharing in-process.** Per-turn `AsyncSqliteSaver.from_conn_string` on one DB file mirrors `worker.py` (fresh event loop per `asyncio.run`, saver never crosses loops; sequential turns, so no concurrent writers). The outer-thread namespace change (`::outer` → `::coder`/`::reviewer`) is required for correctness under a shared saver and is behavior-neutral for every existing checkpointer-less case — but it IS a harness behavior change; if any future case relied on the literal `::outer` string, it breaks (none do today).
4. **Judge client is a bet on `create_model`'s shape.** `create_model(spec).model` returning an invokable `BaseChatModel` is verified against `dac.py:179` usage, but the judge invokes it directly (no DAC agent around it). If `create_model` does something surprising for a bare anthropic spec outside a DAC context (e.g. config.toml side effects, credential fail-fast shape), the fix stays inside `judge.py`. `_message_text` deliberately avoids guessing the AIMessage `.text` API by consuming `.content` the same way `dac.py` does.
5. **Placement-rate denominator is an approximation.** `posted + unattached` treats the reviewer's own "could not be attached" report as ground truth for attempts; a summary comment older than 65k truncation or a reworded `_changes_summary` would silently undercount. Acceptable for a report-only smoke metric; the regexes are pinned to `_changes_summary`'s current deterministic strings.

### Flake-prone cases (watch the run files)

- **L2b `rounds_to_done <= 2`:** an over-strict reviewer planting a blocking finding on a trivial AUTHORS diff burns round 1. Bounded tolerance (≤2) absorbs one such round; the recorded metric shows drift. Same flake profile as R2/R7.
- **R7 majority (2/3)** — unchanged from v1, now fully recorded, so flake analysis is finally possible.
- **D3 judge:** haiku judging "accuracy" of a one-flag README edit is near-deterministic, but rubric wording matters; a red D3 should be triaged by reading the recorded diff/README before blaming the judge. Judge parse failures raise `JudgeError` (loud), never a silent pass.
- **L2a wall time:** up to 6 live turns in one pytest case (10–20+ min). No pytest timeout is configured in this repo; if runs wedge, a per-case timeout is a follow-up, not part of this plan.

### Open questions

1. **`create_model` credential fail-fast:** confirm during Task 4 that a missing `ANTHROPIC_API_KEY` raises inside `judge()` (guarded by `requires_judge` anyway) and that a machine-global `~/.deepagents/config.toml` with codex-only params does not perturb a bare anthropic `create_model` call. If `create_model` proves unusable for a judge-shaped call, the fallback question is whether to promote `langchain-anthropic` to a direct dev dep — flag loudly, do not decide unilaterally.
2. **Should `make eval` exclude L2 by default?** The L2 cases dominate cost/wall time. This plan keeps `make eval` = everything (one command, one truth) and relies on `-k` for iteration; revisit after the first few run files quantify the cost (e.g. add `eval-fast` if it hurts).
3. **`max_rounds=3` vs the kernel's `rework_cap`:** the loop bound is a case-level analogue, not read from config (the kernel default may differ). If cost-per-approved-PR should mirror the deployed `rework_cap` exactly, thread it through later; 3 is the spec'd bound for v2.
4. **R7 consistency rows vs the report's join:** R7 now emits 3 metric rows + 1 status row; `report.py` sums tokens across rows per test (correct for cost) but shows all outcomes concatenated — acceptable, but a per-row view flag on `report.py` may be worth adding once someone actually tunes budgets.
5. **`latest.jsonl` symlink migration:** first v2 run unlinks the v1 regular file (its history is lost unless manually renamed first). One-line note could be added to the README if that history matters — assumed disposable.
