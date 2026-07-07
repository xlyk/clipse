"""Eval-suite fixtures: gh shim on PATH, per-case metrics recording."""
from __future__ import annotations

import json
import os
import time
from collections.abc import Callable
from pathlib import Path
from typing import Any

import pytest

from clipse_agent.contract import WorkerResult

_SHIM_DIR = Path(__file__).parent / "gh_shim"
_RESULTS_DIR = Path(__file__).parent / "results"


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


@pytest.fixture
def record_result(request: pytest.FixtureRequest) -> Callable[..., None]:
    """Append one JSONL metrics row per eval case to results/latest.jsonl."""
    _RESULTS_DIR.mkdir(exist_ok=True)
    out = _RESULTS_DIR / "latest.jsonl"

    def _record(result: WorkerResult, **extra: Any) -> None:
        row = {
            "test": request.node.nodeid,
            "ts": time.time(),
            "outcome": result.outcome.value,
            "block_kind": result.block_kind.value if result.block_kind else None,
            "tokens_in": result.tokens.in_,
            "tokens_out": result.tokens.out,
            "turn_count": result.turn_count,
            **extra,
        }
        with out.open("a") as f:
            f.write(json.dumps(row) + "\n")

    return _record
