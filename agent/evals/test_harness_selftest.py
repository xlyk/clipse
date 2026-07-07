"""Harness sanity — no model, no cost. Proves the fixture-repo git plumbing
and the gh shim behave before any live eval spends tokens."""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

from clipse_agent.contract import Lane, Outcome, Tokens, WorkerResult

import conftest as evals_conftest
import report
from harness import advance_base, commit_on_branch, git_out, make_fixture_repo, seed_pr


def test_fixture_repo_roundtrip(tmp_path: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    assert (repo.worktree / ".git").exists()
    assert git_out(repo.worktree, "rev-parse", "--abbrev-ref", "HEAD") == repo.branch
    commit_on_branch(repo, {"a.txt": "hi\n"}, "add a")
    assert git_out(repo.worktree, "rev-list", "--count", f"origin/{repo.base_branch}..HEAD") == "1"
    advance_base(repo, {"b.txt": "base moved\n"})
    git_out(repo.worktree, "fetch", "origin", repo.base_branch)
    assert git_out(repo.worktree, "rev-list", "--count", f"HEAD..origin/{repo.base_branch}") == "1"


def test_gh_shim_pr_lifecycle(tmp_path: Path, eval_env: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    env = os.environ.copy()

    view = subprocess.run(
        ["gh", "pr", "view", repo.branch, "--json", "url"],
        cwd=repo.worktree, capture_output=True, text=True, env=env,
    )
    assert view.returncode == 1

    create = subprocess.run(
        ["gh", "pr", "create", "--draft", "--head", repo.branch, "--base", repo.base_branch,
         "--title", "t", "--body", "b"],
        cwd=repo.worktree, capture_output=True, text=True, env=env,
    )
    assert create.returncode == 0
    assert "pull/1" in create.stdout

    view_again = subprocess.run(
        ["gh", "pr", "view", repo.branch, "--json", "number,headRefOid,url"],
        cwd=repo.worktree, capture_output=True, text=True, env=env,
    )
    assert view_again.returncode == 0
    pr = json.loads(view_again.stdout)
    assert pr["number"] == 1
    assert pr["headRefOid"] == git_out(repo.worktree, "rev-parse", "HEAD")


def test_seed_pr_matches_branch_head(tmp_path: Path, eval_env: Path) -> None:
    repo = make_fixture_repo(tmp_path, files={"README.md": "# demo\n"})
    seed_pr(eval_env, repo)
    pr = json.loads((eval_env / "pr.json").read_text())
    assert pr["headRefOid"] == git_out(repo.worktree, "rev-parse", "HEAD")


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
