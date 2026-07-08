"""Eval-suite fixtures: gh shim on PATH, per-run metrics recording."""
from __future__ import annotations

import json
import os
import time
from collections.abc import Callable, Iterator
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import pytest

from clipse_agent.contract import WorkerResult

_SHIM_DIR = Path(__file__).parent / "gh_shim"
_RESULTS_DIR = Path(__file__).parent / "results"


def _find_repo_root(start: Path) -> Path:
    """Walk up from `start` to the nearest ancestor containing `.git`."""
    for candidate in (start, *start.parents):
        if (candidate / ".git").exists():
            return candidate
    raise RuntimeError(f"could not locate repo root (no .git) above {start}")


def _listdir(path: Path) -> set[str]:
    return set(os.listdir(path))


def _new_entries(before: set[str], path: Path) -> set[str]:
    """Names added to `path` (non-recursive) since `before` was snapshotted,
    minus expected churn: dotfiles (`.pytest_cache`, `.DS_Store`, ...) and
    `__pycache__`."""
    after = _listdir(path)
    added = after - before
    return {name for name in added if not name.startswith(".") and name != "__pycache__"}


_REPO_ROOT = _find_repo_root(Path(__file__).resolve())
_AGENT_DIR = _REPO_ROOT / "agent"

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


@pytest.fixture(autouse=True)
def _stray_file_guard() -> Iterator[None]:
    """Fail loudly if a live turn writes outside its fixture repo.

    DAC's local shell/filesystem backends anchor at `cwd` but do not jail it
    (`virtual_mode=False` is hardcoded in `create_cli_agent` and not exposed
    -- see .superpowers/sdd/sandbox-escape-investigation.md): an absolute
    path or a `..`-escape in any tool call executes for real wherever it
    points, regardless of `cwd`. This has bitten us once already (a stray
    `AUTHORS` file landed at the checkout root). Deterministic, keyless,
    zero-cost: snapshot the checkout root and `agent/` before the test,
    diff after.
    """
    before_root = _listdir(_REPO_ROOT)
    before_agent = _listdir(_AGENT_DIR)
    yield
    new = _new_entries(before_root, _REPO_ROOT) | _new_entries(before_agent, _AGENT_DIR)
    if new:
        pytest.fail(f"stray files escaped the eval sandbox: {sorted(new)}")


def _run_file() -> Path:
    global _RUN_FILE
    if _RUN_FILE is None:
        _RESULTS_DIR.mkdir(exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        # PID suffix: two pytest processes started in the same second (e.g.
        # a manual -k run alongside a cron sweep) must not collide on one
        # run file.
        _RUN_FILE = _RESULTS_DIR / f"run-{stamp}-{os.getpid()}.jsonl"
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
