"""Harness sanity — no model, no cost. Proves the fixture-repo git plumbing
and the gh shim behave before any live eval spends tokens."""
from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

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
