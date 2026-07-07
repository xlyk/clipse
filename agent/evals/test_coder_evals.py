"""Coder-lane live evals. Each case pins a clipse behavior or incident —
see docs/plans/2026-07-06-agent-evals-implementation-plan.md's incident index."""
from __future__ import annotations

import subprocess
from pathlib import Path

import pytest

from clipse_agent.contract import Outcome
from harness import (
    advance_base,
    commit_on_branch,
    git_out,
    make_fixture_repo,
    requires_anthropic,
    run_coder_turn,
    seed_pr,
)

pytestmark = [pytest.mark.eval, requires_anthropic]

_CALC_BUGGY = "def total(xs):\n    result = 0\n    for i in range(len(xs) - 1):\n        result += xs[i]\n    return result\n"
_CALC_FIXED_CHECK = ["python3", "-c", "import calc; assert calc.total([1, 2, 3]) == 6"]


def _branch_commits(repo) -> int:
    return int(git_out(repo.worktree, "rev-list", "--count", f"origin/{repo.base_branch}..HEAD"))


def test_c1_smoke_small_fix(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(
        tmp_path,
        files={
            "calc.py": _CALC_BUGGY,
            "README.md": "# calc\n`total(xs)` sums a list.\n",
        },
    )
    result = run_coder_turn(
        repo,
        "EVAL-1: total() returns the wrong sum.\n\n"
        "`calc.total([1, 2, 3])` returns 3, expected 6 — the loop drops the "
        "last element. Fix `total` in calc.py so it sums every element.",
    )
    record_result(result)

    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.example/fake/pull/1"
    assert (eval_env / "pr.json").exists()
    assert _branch_commits(repo) >= 1
    # The fix actually works.
    check = subprocess.run(_CALC_FIXED_CHECK, cwd=repo.worktree, capture_output=True, text=True)
    assert check.returncode == 0, check.stderr
    # The graph's commit-message contract held (issue-id prefix from the tail's TITLE).
    subject = git_out(repo.worktree, "log", "-1", "--format=%s")
    assert subject.startswith("EVAL-1:")
    # The branch was actually pushed.
    assert git_out(repo.worktree, "rev-parse", "HEAD") == git_out(
        repo.worktree, "rev-parse", f"origin/{repo.branch}"
    )


# REF-1 regression: a trivial scaffold task once burned 2.02M input tokens.
# Budget rationale: a healthy sonnet turn on this task lands well under 300k
# cumulative input; 500k catches an exploration/retry loop while leaving slack
# for model drift. Tune against results/latest.jsonl after a few runs.
_C2_TOKENS_IN_BUDGET = 500_000
_C2_TOKENS_OUT_BUDGET = 25_000


def test_c2_token_discipline_trivial_task(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# empty project\n"})
    result = run_coder_turn(
        repo,
        "EVAL-1: add a Makefile with a `hello` target that prints hello.\n\n"
        "Create `Makefile` at the repo root with a single phony target "
        "`hello` that runs `echo hello`. Nothing else.",
    )
    record_result(result, budget_in=_C2_TOKENS_IN_BUDGET)

    assert result.outcome == Outcome.needs_review
    assert (repo.worktree / "Makefile").exists()
    assert result.tokens.in_ < _C2_TOKENS_IN_BUDGET, f"token blowup: {result.tokens.in_} in"
    assert result.tokens.out < _C2_TOKENS_OUT_BUDGET, f"token blowup: {result.tokens.out} out"


def _rework_repo(tmp_path: Path, eval_env: Path):
    """Branch already carries the buggy commit + an open PR — a card sitting
    in rework, exactly what the dispatcher re-runs the coder against."""
    repo = make_fixture_repo(
        tmp_path,
        files={"README.md": "# calc\n`total(xs)` sums a list.\n", "calc.py": "def total(xs):\n    return sum(xs)\n"},
    )
    commit_on_branch(repo, {"calc.py": _CALC_BUGGY}, "EVAL-1: rewrite total loop")
    seed_pr(eval_env, repo)
    return repo


def test_c5_rework_with_specific_feedback(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = _rework_repo(tmp_path, eval_env)
    head_before = git_out(repo.worktree, "rev-parse", "HEAD")
    result = run_coder_turn(
        repo,
        "EVAL-1: rewrite total() as an explicit loop.",
        review_feedback=(
            "VERDICT: CHANGES_REQUESTED\n"
            "- calc.py:3: blocking: `range(len(xs) - 1)` drops the last element; "
            "total([1,2,3]) returns 3, expected 6. Iterate the full list."
        ),
    )
    record_result(result)

    assert result.outcome == Outcome.needs_review
    # CLI-15 regression: the rework turn must actually change the diff.
    assert git_out(repo.worktree, "rev-parse", "HEAD") != head_before
    check = subprocess.run(_CALC_FIXED_CHECK, cwd=repo.worktree, capture_output=True, text=True)
    assert check.returncode == 0, check.stderr


def test_c6_rework_with_vague_feedback(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = _rework_repo(tmp_path, eval_env)
    head_before = git_out(repo.worktree, "rev-parse", "HEAD")
    result = run_coder_turn(
        repo,
        "EVAL-1: rewrite total() as an explicit loop.",
        review_feedback="The diff did not change; same findings as before.",
    )
    changed = git_out(repo.worktree, "rev-parse", "HEAD") != head_before
    record_result(result, diff_changed=changed)

    # Vague feedback has two honest outcomes: find + fix the defect anyway,
    # or block asking what the findings were. What it must never do is claim
    # a review-ready change while committing nothing (the CLI-15 dead loop).
    assert result.outcome in (Outcome.needs_review, Outcome.blocked)
    if result.outcome == Outcome.needs_review:
        assert changed, "needs_review with an unchanged diff is the CLI-15 dead loop"
