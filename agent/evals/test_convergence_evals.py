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
    # None (not a vacuous True) when convergence took a single round: there
    # was no feedback in play to have addressed.
    feedback_addressed = (
        None if len(out.rounds) < 2
        else out.rounds[1].coder_head != out.rounds[0].coder_head
    )
    reviewer_summaries = [r.reviewer.summary[:500] for r in out.rounds if r.reviewer is not None]
    record_result(
        last.reviewer or last.coder,
        rounds_to_done=out.rounds_to_done,
        round_outcomes=out.round_outcomes,
        loop_tokens_in=out.tokens_in,
        loop_tokens_out=out.tokens_out,
        feedback_addressed=feedback_addressed,
        reviewer_summaries=reviewer_summaries,
    )

    assert out.rounds_to_done is not None, f"never converged: {out.round_outcomes}"
    assert out.rounds_to_done <= 3
    # The planted defect is gone by convergence, whichever round fixed it.
    check = subprocess.run(_CALC_FIXED_CHECK, cwd=repo.worktree, capture_output=True, text=True)
    assert check.returncode == 0, check.stderr
    # R6 actionability: a rework round that shipped a byte-identical diff is
    # the CLI-15 dead loop surfacing at loop level -- the reviewer's summary
    # did not carry enough signal (AGENTS.md open follow-up made measurable).
    # Only a fault when feedback genuinely existed and went unaddressed --
    # None (converged round 1, nothing to address) is not a failure.
    assert feedback_addressed is not False, "round-2 coder did not change the diff after changes_requested"
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
    reviewer_summaries = [r.reviewer.summary[:500] for r in out.rounds if r.reviewer is not None]
    record_result(
        last.reviewer or last.coder,
        rounds_to_done=out.rounds_to_done,
        round_outcomes=out.round_outcomes,
        loop_tokens_in=out.tokens_in,
        loop_tokens_out=out.tokens_out,
        reviewer_summaries=reviewer_summaries,
    )

    assert out.rounds_to_done is not None, f"never converged: {out.round_outcomes}"
    # Expect 1; tolerate one extra round (an over-strict blocking finding on a
    # trivial diff is reviewer-calibration signal -- visible in the recorded
    # metric -- not a harness failure). >2 fails: that is a broken loop.
    assert out.rounds_to_done <= 2, f"clean trivial PR burned {out.rounds_to_done} review rounds"
    # Deterministic honesty checks: convergence alone doesn't prove the task
    # was actually done -- confirm the file landed with the right content and
    # that a PR was genuinely opened along the way.
    authors = repo.worktree / "AUTHORS"
    assert authors.exists(), "converged without ever creating AUTHORS"
    assert authors.read_text().strip() == "clipse evals"
    assert (eval_env / "pr.json").exists(), "converged without ever opening a PR"
