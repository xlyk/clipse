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
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

from clipse_agent.contract import Outcome, WorkerResult
from clipse_agent.graphs.coder import build_coder_graph
from clipse_agent.graphs.reviewer import build_reviewer_graph
from clipse_agent.profiles.coder import get_coder_docs_profile, get_coder_profile
from clipse_agent.profiles.reviewer import get_reviewer_profile

# Override the lane model for a whole eval run, e.g.
#   CLIPSE_EVAL_MODEL=openai_codex:gpt-5-codex make eval
EVAL_MODEL = os.environ.get("CLIPSE_EVAL_MODEL") or None

_needs_anthropic_key = (EVAL_MODEL is None or EVAL_MODEL.startswith("anthropic:")) and not os.environ.get(
    "ANTHROPIC_API_KEY"
)
requires_anthropic = pytest.mark.skipif(
    _needs_anthropic_key, reason="ANTHROPIC_API_KEY required for live evals (source ~/.secrets)"
)

# The docs-accuracy judge is pinned to an anthropic model regardless of
# CLIPSE_EVAL_MODEL, so it needs the key even on a codex-matrix run.
requires_judge = pytest.mark.skipif(
    not os.environ.get("ANTHROPIC_API_KEY"),
    reason="LLM judge is pinned to anthropic:claude-haiku-4-5; ANTHROPIC_API_KEY required",
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
    def round_outcomes(self) -> list[list[str | None]]:
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
