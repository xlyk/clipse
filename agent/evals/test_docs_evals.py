"""Docs-substep evals: the coder graph's run_docs turn rides every clean
coder turn, so these drive the FULL coder graph and grade only the docs
outcome — did documentation get updated when warranted (D1) and left alone
when not (D2)."""
from __future__ import annotations

import hashlib
from pathlib import Path

import pytest

from clipse_agent.contract import Outcome
from harness import make_fixture_repo, requires_anthropic, run_coder_turn

pytestmark = [pytest.mark.eval, requires_anthropic]

_CLI_FILES = {
    "cli.py": (
        "import argparse\n\n\n"
        "def build_parser():\n"
        "    parser = argparse.ArgumentParser(prog='demo')\n"
        "    parser.add_argument('--name', default='world')\n"
        "    return parser\n"
    ),
    "README.md": (
        "# demo\n\n## Flags\n\n- `--name <name>` — who to greet (default: world)\n"
    ),
}


def test_d1_user_facing_change_updates_docs(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(tmp_path, files=_CLI_FILES)
    result = run_coder_turn(
        repo,
        "EVAL-1: add a --shout flag to cli.py.\n\n"
        "Add a boolean `--shout` flag (store_true) to the parser in cli.py.",
    )
    record_result(result)

    assert result.outcome == Outcome.needs_review
    readme = (repo.worktree / "README.md").read_text()
    assert "--shout" in readme, "user-facing flag added but README's Flags section not updated"


def test_d2_internal_change_leaves_docs_alone(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(tmp_path, files=_CLI_FILES)
    readme_before = hashlib.sha256((repo.worktree / "README.md").read_bytes()).hexdigest()
    result = run_coder_turn(
        repo,
        "EVAL-1: rename build_parser's local variable.\n\n"
        "In cli.py, rename the local variable `parser` to `arg_parser`. "
        "Pure internal rename; no behavior change, no interface change.",
    )
    record_result(result)

    assert result.outcome == Outcome.needs_review
    readme_after = hashlib.sha256((repo.worktree / "README.md").read_bytes()).hexdigest()
    assert readme_before == readme_after, "docs step invented busywork on an internal-only change"
