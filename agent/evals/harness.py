"""Shared harness for clipse agent evals.

Evals drive the REAL graphs (`build_coder_graph`/`build_reviewer_graph`) with
a REAL model and REAL git, against a throwaway fixture repo whose "GitHub" is
a local bare repo (the git remote) plus the fake `gh` shim on PATH (PRs and
comments). Deterministic graders only: outcome enums, git state, token
budgets, shim call logs.
"""
from __future__ import annotations

import asyncio
import json
import os
import subprocess
import uuid
from dataclasses import dataclass
from pathlib import Path

import pytest

from clipse_agent.contract import WorkerResult
from clipse_agent.graphs.coder import build_coder_graph
from clipse_agent.graphs.reviewer import build_reviewer_graph
from clipse_agent.profiles.coder import get_coder_docs_profile, get_coder_profile
from clipse_agent.profiles.reviewer import get_reviewer_profile
from clipse_agent.transcript import TranscriptWriter

# Override the lane model for a whole eval run, e.g.
#   CLIPSE_EVAL_MODEL=openai_codex:gpt-5-codex make eval
EVAL_MODEL = os.environ.get("CLIPSE_EVAL_MODEL") or None

_needs_anthropic_key = (EVAL_MODEL is None or EVAL_MODEL.startswith("anthropic:")) and not os.environ.get(
    "ANTHROPIC_API_KEY"
)
requires_anthropic = pytest.mark.skipif(
    _needs_anthropic_key, reason="ANTHROPIC_API_KEY required for live evals (source ~/.secrets)"
)


@dataclass(frozen=True)
class FixtureRepo:
    origin: Path
    worktree: Path
    base_branch: str
    branch: str


def git_out(cwd: Path, *args: str) -> str:
    proc = subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True)
    if proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed (exit {proc.returncode}): {proc.stderr}")
    return proc.stdout.strip()


def _configure_identity(repo_dir: Path) -> None:
    git_out(repo_dir, "config", "user.email", "evals@clipse.local")
    git_out(repo_dir, "config", "user.name", "clipse evals")


def make_fixture_repo(
    tmp_path: Path,
    *,
    files: dict[str, str],
    base_branch: str = "main",
    branch: str = "clipse/EVAL-1",
) -> FixtureRepo:
    origin = tmp_path / "origin.git"
    subprocess.run(
        ["git", "init", "--bare", "-b", base_branch, str(origin)], check=True, capture_output=True
    )
    worktree = tmp_path / "worktree"
    git_out(tmp_path, "clone", str(origin), str(worktree))
    _configure_identity(worktree)
    for rel, content in files.items():
        target = worktree / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
    git_out(worktree, "add", "-A")
    git_out(worktree, "commit", "-m", "init")
    git_out(worktree, "push", "-u", "origin", base_branch)
    git_out(worktree, "checkout", "-b", branch)
    git_out(worktree, "push", "-u", "origin", branch)
    return FixtureRepo(origin=origin, worktree=worktree, base_branch=base_branch, branch=branch)


def commit_on_branch(repo: FixtureRepo, files: dict[str, str], message: str) -> None:
    for rel, content in files.items():
        target = repo.worktree / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
    git_out(repo.worktree, "add", "-A")
    git_out(repo.worktree, "commit", "-m", message)
    git_out(repo.worktree, "push", "origin", repo.branch)


def advance_base(repo: FixtureRepo, files: dict[str, str], message: str = "advance base") -> None:
    clone = repo.origin.parent / f"base-writer-{uuid.uuid4().hex[:8]}"
    git_out(repo.origin.parent, "clone", str(repo.origin), str(clone))
    _configure_identity(clone)
    git_out(clone, "checkout", repo.base_branch)
    for rel, content in files.items():
        target = clone / rel
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
    git_out(clone, "add", "-A")
    git_out(clone, "commit", "-m", message)
    git_out(clone, "push", "origin", repo.base_branch)


def seed_pr(gh_dir: Path, repo: FixtureRepo) -> None:
    head = git_out(repo.worktree, "rev-parse", "HEAD")
    gh_dir.mkdir(parents=True, exist_ok=True)
    (gh_dir / "pr.json").write_text(
        json.dumps({"url": "https://github.example/fake/pull/1", "number": 1, "headRefOid": head})
    )


def _input_state(repo: FixtureRepo, issue_text: str, *, max_tokens: int, thread_id: str) -> dict:
    return {
        "issue_id": "EVAL-1",
        "run_id": "eval-run-1",
        "thread_id": thread_id,
        "workspace": str(repo.worktree),
        "base_branch": repo.base_branch,
        "issue_text": issue_text,
        "max_tokens": max_tokens,
    }


def run_coder_turn(
    repo: FixtureRepo,
    issue_text: str,
    *,
    review_feedback: str = "",
    max_tokens: int = 400_000,
    thread_id: str = "eval-thread",
    transcript_path: str = "",
) -> WorkerResult:
    graph = build_coder_graph(
        profile=get_coder_profile(EVAL_MODEL),
        docs_profile=get_coder_docs_profile(EVAL_MODEL),
        transcript=TranscriptWriter(transcript_path) if transcript_path else None,
    )
    state = _input_state(repo, issue_text, max_tokens=max_tokens, thread_id=thread_id)
    if review_feedback:
        state["review_feedback"] = review_feedback
    config = {"configurable": {"thread_id": f"{thread_id}::outer"}}
    final = asyncio.run(graph.ainvoke(state, config))
    return final["result"]


def run_reviewer_turn(
    repo: FixtureRepo,
    issue_text: str,
    *,
    max_tokens: int = 400_000,
    thread_id: str = "eval-thread",
) -> WorkerResult:
    graph = build_reviewer_graph(profile=get_reviewer_profile(EVAL_MODEL))
    state = _input_state(repo, issue_text, max_tokens=max_tokens, thread_id=thread_id)
    config = {"configurable": {"thread_id": f"{thread_id}::outer"}}
    final = asyncio.run(graph.ainvoke(state, config))
    return final["result"]
