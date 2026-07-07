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


@pytest.fixture
def eval_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Route every `gh` call (graph nodes AND the DAC agent's shell) to the
    shim, give it a per-test state dir, and scrub the CLIPSE_* env fallbacks
    so a dev shell's leftovers can't leak into a case's prompt."""
    gh_dir = tmp_path / "gh-state"
    monkeypatch.setenv("CLIPSE_EVAL_GH_DIR", str(gh_dir))
    monkeypatch.setenv("PATH", f"{_SHIM_DIR}:{os.environ['PATH']}")
    for var in ("CLIPSE_ISSUE_TEXT", "CLIPSE_REVIEW_FEEDBACK", "CLIPSE_DEPENDENCY_NOTES"):
        monkeypatch.delenv(var, raising=False)
    return gh_dir


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
