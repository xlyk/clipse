"""Tests for the Reviewer lane's LangGraph graph (`clipse_agent.graphs.reviewer`).

DAC (`dac.build_coder_agent` / `dac.drive_turn`) and git/gh are always faked
here via the graph's own dependency-injection seams (`agent_factory`,
`turn_driver`, `run_command`) -- these tests never touch a real model, a
real DAC agent, or a real subprocess/network call. `run_DAC` is the graph's
only async node, so the compiled graph must be driven with `.ainvoke`/
`.astream`; per `test_dac.py`'s convention, plain `asyncio.run` drives it
(no pytest-asyncio in this repo's approved dev deps). Fixtures mirror
`test_coder_graph.py`'s, adapted for the Reviewer lane's own gh call shapes.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable, Sequence
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver

from clipse_agent import dac
from clipse_agent.contract import BlockKind, Lane, Outcome, WorkerResult
from clipse_agent.dac import DacTurnResult
from clipse_agent.graphs import coder, reviewer
from clipse_agent.profiles.reviewer import get_reviewer_profile

# ---------------------------------------------------------------------------
# Fakes / fixtures
# ---------------------------------------------------------------------------


def _worktree(tmp_path: Path) -> str:
    """Build a fake worktree dir: just a directory with a `.git` marker.

    `ensure_worktree` (reused from `graphs.coder`) only ever checks for
    existence + a `.git` entry -- it never shells out to `git` to verify a
    real repo -- so no real git repo is needed here.
    """
    work = tmp_path / "worktree"
    work.mkdir()
    (work / ".git").write_text("gitdir: /fake/main/.git/worktrees/spac-1\n")
    return str(work)


class _RunCall:
    __slots__ = ("argv", "cwd")

    def __init__(self, argv: list[str], cwd: str) -> None:
        self.argv = argv
        self.cwd = cwd

    def __repr__(self) -> str:
        return f"_RunCall(argv={self.argv!r}, cwd={self.cwd!r})"


def _starts_with(*prefix: str) -> Callable[[list[str]], bool]:
    return lambda argv: argv[: len(prefix)] == list(prefix)


class FakeRunner:
    """Injectable stand-in for `coder.CommandRunner` (reused type).

    Matches each call against `rules` (checked in order); the first
    matching predicate wins. A call matching nothing gets `default` -- a
    clean no-output success -- so a test only has to script the calls it
    actually cares about. Every call is recorded in `calls`, in order.
    """

    def __init__(
        self,
        rules: Sequence[tuple[Callable[[list[str]], bool], coder.CommandResult]] = (),
        default: coder.CommandResult | None = None,
    ) -> None:
        self.rules = list(rules)
        self.default = default or coder.CommandResult(returncode=0, stdout="", stderr="")
        self.calls: list[_RunCall] = []

    def __call__(self, argv: Sequence[str], cwd: str) -> coder.CommandResult:
        argv_list = list(argv)
        self.calls.append(_RunCall(argv_list, cwd))
        for predicate, result in self.rules:
            if predicate(argv_list):
                return result
        return self.default


def _base_runner(
    *,
    branch: str = "clipse/spac-1",
    pr_number: int = 42,
    commit_sha: str = "abc123",
    pr_url: str = "https://github.com/acme/widgets/pull/42",
) -> FakeRunner:
    return FakeRunner(
        rules=[
            (
                _starts_with("git", "rev-parse", "--abbrev-ref", "HEAD"),
                coder.CommandResult(0, f"{branch}\n", ""),
            ),
            (
                _starts_with("gh", "pr", "view"),
                coder.CommandResult(
                    0,
                    json.dumps({"number": pr_number, "headRefOid": commit_sha, "url": pr_url}),
                    "",
                ),
            ),
        ],
    )


def _fake_agent_factory(calls: list[dict[str, Any]]) -> Callable[..., tuple[str, str]]:
    def factory(profile: Any, checkpointer: Any, cwd: str) -> tuple[str, str]:
        calls.append({"profile": profile, "checkpointer": checkpointer, "cwd": cwd})
        return "fake-agent-graph", "fake-backend"

    return factory


def _fake_turn_driver(result: DacTurnResult, calls: list[dict[str, Any]]) -> Callable[..., Any]:
    async def driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        calls.append({"agent_graph": agent_graph, "config": config, **kwargs})
        return result

    return driver


async def _drive(graph: Any, input_state: dict[str, Any], config: dict[str, Any]) -> tuple[list[str], WorkerResult]:
    """Run `graph` once via `.astream(..., stream_mode="updates")`.

    Returns (node execution order, the WorkerResult `emit_result`
    produced). Only `emit_result` ever writes the `result` key.
    """
    order: list[str] = []
    result: WorkerResult | None = None
    async for update in graph.astream(input_state, config, stream_mode="updates"):
        node, partial = next(iter(update.items()))
        order.append(node)
        if partial and "result" in partial:
            result = partial["result"]
    assert result is not None, f"emit_result never ran; node order was {order}"
    return order, result


def _assert_valid_result(result: WorkerResult, *, blocked: bool) -> None:
    """Every emitted result must validate as contract.WorkerResult, and
    block_kind must be present iff outcome == blocked (amendment X2) --
    both on the object itself and after a `by_alias, exclude_none` dump,
    which is exactly how `worker.py` serializes a result to stdout.
    """
    assert isinstance(result, WorkerResult)
    dumped = result.model_dump_json(by_alias=True, exclude_none=True)
    raw = json.loads(dumped)
    reparsed = WorkerResult.model_validate_json(dumped)
    assert reparsed == result

    if blocked:
        assert result.outcome == Outcome.blocked
        assert result.block_kind is not None
        assert raw["block_kind"] == result.block_kind.value
    else:
        assert result.outcome != Outcome.blocked
        assert result.block_kind is None
        assert "block_kind" not in raw


# ---------------------------------------------------------------------------
# Happy path: PASS verdict -> done, full node order, no gh calls at all
# ---------------------------------------------------------------------------


def test_pass_verdict_runs_full_node_order_and_emits_done(tmp_path):
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="completed",
        final_text="Checked the diff; the implementation is correct and well-tested.\n\nVERDICT: PASS",
        tokens_in=120,
        tokens_out=340,
    )

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )

    workspace = _worktree(tmp_path)
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "workspace": workspace,
        "issue_text": "Build the widget factory.",
    }
    config = {"configurable": {"thread_id": "thread-1"}}

    order, result = asyncio.run(_drive(graph, input_state, config))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "classify", "emit_result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.lane == Lane.reviewer
    assert result.run_id == "run-1"
    assert result.issue_id == "SPAC-1"
    assert result.thread_id == "thread-1"
    assert result.turn_count == 1
    assert result.tokens.in_ == 120
    assert result.tokens.out == 340
    assert result.artifacts == []

    # A PASS never touches gh itself -- only DAC's own (mocked-away) shell
    # tool reviews the diff; the wrapping graph posts nothing.
    gh_calls = [c for c in runner.calls if c.argv[0] == "gh"]
    assert gh_calls == []


# ---------------------------------------------------------------------------
# load_diff: the PR diff is pre-computed into task_text (so the reviewer sees
# it in-context, never depending on the agent shelling out `git diff` -- the
# live Phase-3 smoke rejected that via the read-mostly allow-list and passed
# a PR blind).
# ---------------------------------------------------------------------------


def test_load_diff_injects_pr_diff_into_task_text(tmp_path):
    diff_text = (
        "diff --git a/HELLO.md b/HELLO.md\n"
        "--- /dev/null\n+++ b/HELLO.md\n@@ -0,0 +1 @@\n"
        "+Clipse turns Linear issues into merged PRs.\n"
    )
    runner = _base_runner()
    runner.rules.insert(0, (_starts_with("git", "diff"), coder.CommandResult(0, diff_text, "")))
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-9",
        "run_id": "run-1",
        "thread_id": "thread-9",
        "workspace": _worktree(tmp_path),
        "issue_text": "Add HELLO.md with the tagline.",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-9"}}))

    # load_diff ran the diff command (default base "main")...
    assert any(c.argv[:2] == ["git", "diff"] and "main...HEAD" in c.argv for c in runner.calls)
    # ...and the DAC turn was driven with a task_text that carries BOTH the
    # original issue text AND the diff content, in a ```diff fence.
    task_text = turn_calls[0]["task_text"]
    assert "Add HELLO.md with the tagline." in task_text
    assert "Clipse turns Linear issues into merged PRs." in task_text
    assert "```diff" in task_text


def test_load_diff_degrades_gracefully_when_git_diff_fails(tmp_path):
    runner = _base_runner()
    runner.rules.insert(0, (_starts_with("git", "diff"), coder.CommandResult(128, "", "fatal: bad revision 'main...HEAD'")))
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-9b",
        "run_id": "run-1",
        "thread_id": "thread-9b",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    # A failing `git diff` must NOT raise -- the review still runs, with a note.
    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-9b"}}))
    assert "load_diff" in order
    assert "could not compute" in turn_calls[0]["task_text"]
    assert result.outcome == Outcome.done


def test_load_diff_truncated_names_the_omitted_files(tmp_path):
    # A diff over the cap: first.py's `diff --git` header is inside the kept
    # prefix, but second.py/third.py's headers fall beyond it. The task text
    # must end with a DIFF TRUNCATED section naming the cut files and telling
    # the reviewer to read each one (the 60k cap silently dropped three files
    # from one Reflex review).
    first = (
        "diff --git a/first.py b/first.py\n--- a/first.py\n+++ b/first.py\n"
        "@@ -0,0 +1,12000 @@\n" + ("+line\n" * 12000)
    )
    second = "diff --git a/second.py b/second.py\n--- a/second.py\n+++ b/second.py\n@@ -0,0 +1 @@\n+hi\n"
    third = "diff --git a/third.py b/third.py\n--- a/third.py\n+++ b/third.py\n@@ -0,0 +1 @@\n+yo\n"
    diff_text = first + second + third
    assert len(diff_text) > reviewer._MAX_DIFF_CHARS

    runner = _base_runner()
    runner.rules.insert(
        0,
        (_starts_with("git", "diff", "--name-only"), coder.CommandResult(0, "first.py\nsecond.py\nthird.py\n", "")),
    )
    runner.rules.insert(1, (_starts_with("git", "diff"), coder.CommandResult(0, diff_text, "")))
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-15",
        "run_id": "run-1",
        "thread_id": "thread-15",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-15"}}))

    task_text = turn_calls[0]["task_text"]
    assert "DIFF TRUNCATED" in task_text
    section = task_text[task_text.index("DIFF TRUNCATED") :]
    # cut files are named; the fully-kept first file is not
    assert "- second.py" in section
    assert "- third.py" in section
    assert "- first.py" not in section
    # instructs per-file reads, and the section is the tail of the task text
    assert "git diff main...HEAD -- <file>" in section
    assert task_text.rstrip().endswith("- third.py")


def test_load_diff_small_diff_has_no_truncation_note(tmp_path):
    runner = _base_runner()
    runner.rules.insert(0, (_starts_with("git", "diff"), coder.CommandResult(0, "diff --git a/x b/x\n+one line\n", "")))
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-15b",
        "run_id": "run-1",
        "thread_id": "thread-15b",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-15b"}}))

    assert "DIFF TRUNCATED" not in turn_calls[0]["task_text"]
    # a small diff never even asks for the file list
    assert not any(c.argv[:3] == ["git", "diff", "--name-only"] for c in runner.calls)


# ---------------------------------------------------------------------------
# changes_requested: inline comments posted via gh + a summary review
# ---------------------------------------------------------------------------


def test_changes_requested_posts_inline_comments_and_a_summary_comment(tmp_path):
    runner = _base_runner(pr_number=7, commit_sha="deadbeef", pr_url="https://github.com/acme/widgets/pull/7")
    final_text = (
        "Found a couple of issues.\n\n"
        "VERDICT: CHANGES_REQUESTED\n"
        "- src/thing.py:12: Missing null check before dereferencing user.\n"
        "- src/other.py:30: This mutates the input list; copy it first.\n"
    )
    turn_result = DacTurnResult(outcome_hint="completed", final_text=final_text, tokens_in=10, tokens_out=50)

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-2",
        "run_id": "run-1",
        "thread_id": "thread-2",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-2"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "classify", "post_comments", "emit_result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.changes_requested
    assert result.pr_url == "https://github.com/acme/widgets/pull/7"
    assert result.artifacts == []

    api_calls = [c for c in runner.calls if c.argv[:2] == ["gh", "api"]]
    assert len(api_calls) == 2
    assert any("path=src/thing.py" in c.argv for c in api_calls)
    assert any("line=12" in c.argv for c in api_calls)
    assert any("path=src/other.py" in c.argv for c in api_calls)
    assert any("line=30" in c.argv for c in api_calls)
    assert all(any("commit_id=deadbeef" in a for a in c.argv) for c in api_calls)

    # No FORMAL gh review: the coder and reviewer share one gh identity, and
    # GitHub forbids approving/requesting-changes on your own PR. The verdict
    # flows via the JSON result (drives merging->rework); the summary is posted
    # as a plain PR comment, which IS allowed on your own PR.
    assert not [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "review"]]
    comment_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "comment"]]
    assert len(comment_calls) == 1


def test_changes_requested_posts_summary_comment_when_no_structured_comments_found(tmp_path):
    # The model gave a verdict but no machine-parseable bullet lines -- the PR
    # must still get the summary as a plain comment; it just has no inline
    # per-line comments attached.
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="completed",
        final_text="This isn't quite right yet.\n\nVERDICT: CHANGES_REQUESTED",
        tokens_in=5,
        tokens_out=5,
    )

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-2b",
        "run_id": "run-1",
        "thread_id": "thread-2b",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-2b"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "classify", "post_comments", "emit_result"]
    assert result.outcome == Outcome.changes_requested

    api_calls = [c for c in runner.calls if c.argv[:2] == ["gh", "api"]]
    assert api_calls == []
    assert not [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "review"]]
    comment_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "comment"]]
    assert len(comment_calls) == 1


# ---------------------------------------------------------------------------
# post_comments resilience: no-PR grace + best-effort inline comments
# ---------------------------------------------------------------------------


def test_post_comments_no_pr_is_graceful() -> None:
    runner = FakeRunner(
        rules=[
            (_starts_with("gh", "pr", "view"), coder.CommandResult(1, "", "no pull requests found")),
        ]
    )
    node = reviewer.make_post_comments(runner)
    out = node(
        {
            "branch": "clipse/EVAL-1",
            "cwd": "/tmp",
            "review_comments": [reviewer.InlineComment(path="a.py", line=3, body="x")],
        }
    )
    assert out == {"pr_url": None, "comments_posted": 0, "comments_failed": 0, "failed_comments": []}
    # Nothing after the failed view: no gh api, no gh pr comment.
    assert all(call.argv[:2] != ["gh", "api"] for call in runner.calls)
    assert all(call.argv[:3] != ["gh", "pr", "comment"] for call in runner.calls)


def test_post_comments_pr_view_transient_failure_raises() -> None:
    # A gh failure that is NOT the no-PR case must raise -> blocked/transient
    # -> kernel retry, instead of silently dropping every finding.
    runner = FakeRunner(
        rules=[
            (_starts_with("gh", "pr", "view"), coder.CommandResult(1, "", "connect: network is unreachable")),
        ]
    )
    node = reviewer.make_post_comments(runner)
    with pytest.raises(reviewer.ReviewerGraphError, match="gh pr view"):
        node({"branch": "clipse/EVAL-1", "cwd": "/tmp", "review_comments": []})


def test_post_comments_inline_422_degrades_to_summary() -> None:
    pr_json = json.dumps({"number": 7, "headRefOid": "abc123", "url": "https://x/pull/7"})
    runner = FakeRunner(
        rules=[
            (_starts_with("gh", "pr", "view"), coder.CommandResult(0, pr_json, "")),
            (_starts_with("gh", "api"), coder.CommandResult(1, "", "HTTP 422: Validation Failed")),
        ]
    )
    node = reviewer.make_post_comments(runner)
    out = node(
        {
            "branch": "clipse/EVAL-1",
            "cwd": "/tmp",
            "dac_summary": "found problems",
            "review_comments": [
                reviewer.InlineComment(path="a.py", line=3, body="off-by-one"),
                reviewer.InlineComment(path="b.py", line=9, body="unused import", severity="nit"),
            ],
        }
    )
    assert out["comments_posted"] == 0
    assert out["comments_failed"] == 2
    assert out["pr_url"] == "https://x/pull/7"
    assert out["failed_comments"] == [
        reviewer.InlineComment(path="a.py", line=3, body="off-by-one"),
        reviewer.InlineComment(path="b.py", line=9, body="unused import", severity="nit"),
    ]
    # The summary comment still ran and carries the failed findings inline.
    summary_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "comment"]]
    assert len(summary_calls) == 1
    body = summary_calls[0].argv[summary_calls[0].argv.index("--body") + 1]
    assert "a.py:3" in body and "off-by-one" in body


def test_post_comments_summary_post_failure_does_not_raise() -> None:
    # A rate-limited or otherwise-failing `gh pr comment` for the summary must
    # not raise -- the verdict already reaches the kernel via this run's
    # typed JSON result, so a failed summary post is a best-effort miss, not
    # a run failure.
    pr_json = json.dumps({"number": 7, "headRefOid": "abc123", "url": "https://x/pull/7"})
    runner = FakeRunner(
        rules=[
            (_starts_with("gh", "pr", "view"), coder.CommandResult(0, pr_json, "")),
            (_starts_with("gh", "pr", "comment"), coder.CommandResult(1, "", "rate limited")),
        ]
    )
    node = reviewer.make_post_comments(runner)
    out = node(
        {
            "branch": "clipse/EVAL-1",
            "cwd": "/tmp",
            "dac_summary": "found problems",
            "review_comments": [reviewer.InlineComment(path="a.py", line=3, body="off-by-one")],
        }
    )
    # No exception, and the dict shape is unchanged despite the failed post.
    assert out["pr_url"] == "https://x/pull/7"
    assert out["comments_posted"] == 1
    assert out["comments_failed"] == 0
    comment_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "comment"]]
    assert len(comment_calls) == 1


def test_post_comments_summary_body_is_truncated_to_github_safe_cap() -> None:
    # `_changes_summary` concatenates an unbounded dac_summary; GitHub caps a
    # comment body at 65,536 chars, so an oversized body must be capped
    # before it reaches `gh pr comment` argv.
    pr_json = json.dumps({"number": 7, "headRefOid": "abc123", "url": "https://x/pull/7"})
    runner = FakeRunner(rules=[(_starts_with("gh", "pr", "view"), coder.CommandResult(0, pr_json, ""))])
    node = reviewer.make_post_comments(runner)
    out = node(
        {
            "branch": "clipse/EVAL-1",
            "cwd": "/tmp",
            "dac_summary": "x" * 100_000,
            "review_comments": [],
        }
    )
    assert out["pr_url"] == "https://x/pull/7"
    comment_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "comment"]]
    assert len(comment_calls) == 1
    body = comment_calls[0].argv[comment_calls[0].argv.index("--body") + 1]
    assert len(body) <= reviewer._MAX_COMMENT_CHARS


# ---------------------------------------------------------------------------
# Missing/unparseable verdict -> conservative changes_requested, never PASS
# ---------------------------------------------------------------------------


def test_missing_verdict_is_treated_as_changes_requested_not_pass(tmp_path):
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="completed",
        final_text="I looked at the diff but forgot to give a verdict.",
        tokens_in=5,
        tokens_out=5,
    )

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-2c",
        "run_id": "run-1",
        "thread_id": "thread-2c",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-2c"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "classify", "post_comments", "emit_result"]
    assert result.outcome == Outcome.changes_requested


# ---------------------------------------------------------------------------
# Interrupt -> blocked(needs_input); skips classify and every gh call
# ---------------------------------------------------------------------------


def test_interrupt_emits_blocked_needs_input_and_skips_classify_and_gh(tmp_path):
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="interrupted",
        final_text="paused, needs a decision",
        tokens_in=10,
        tokens_out=5,
        interrupt_payload=[{"action_requests": [{"name": "shell", "args": {"command": "rm -rf /"}}]}],
    )

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-3",
        "run_id": "run-1",
        "thread_id": "thread-3",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-3"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.needs_input
    assert result.pr_url is None
    assert result.tokens.in_ == 10
    assert result.tokens.out == 5

    gh_calls = [c for c in runner.calls if c.argv[0] == "gh"]
    assert gh_calls == []


def test_token_ceiling_exceeded_emits_blocked_capability_even_if_interrupted(tmp_path):
    # token_ceiling_exceeded must win over interrupt_payload (dac.py's own
    # documented precedence -- see graphs/coder.py's identical rule).
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="interrupted",
        final_text="ran out of budget",
        tokens_in=900,
        tokens_out=200,
        interrupt_payload=[{"some": "payload"}],
        token_ceiling_exceeded=True,
    )
    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-4",
        "run_id": "run-1",
        "thread_id": "thread-4",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "max_tokens": 1000,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-4"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability
    assert "1100" in result.summary


# ---------------------------------------------------------------------------
# Resume (continuation after a prior interrupt)
# ---------------------------------------------------------------------------


def test_resume_turn_drives_dac_with_resume_payload_not_task_text(tmp_path):
    runner = _base_runner()
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(
        outcome_hint="completed", final_text="resumed and finished.\n\nVERDICT: PASS", tokens_in=5, tokens_out=5
    )

    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-6",
        "run_id": "run-2",
        "thread_id": "thread-6",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "resume_payload": {"int-1": {"decisions": [{"type": "approve"}]}},
        "turn_count": 1,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-6"}}))

    assert order[:4] == ["load_context", "ensure_worktree", "load_diff", "run_DAC"]
    assert turn_calls[0]["resume"] == {"int-1": {"decisions": [{"type": "approve"}]}}
    assert "task_text" not in turn_calls[0]
    assert result.turn_count == 2


# ---------------------------------------------------------------------------
# DAC agent build: default wiring reaches the real clipse_agent.dac module,
# reusing the same safety-critical create_cli_agent invariants as the Coder
# lane, with the Reviewer's own (distinct, read-mostly) profile.
# ---------------------------------------------------------------------------


def test_build_reviewer_graph_default_wiring_uses_real_dac_module_with_safety_invariants(tmp_path, monkeypatch):
    class _FakeAgentGraph:
        async def astream(self, stream_input: Any, **kwargs: Any):
            yield (
                (),
                "messages",
                (
                    AIMessage(
                        content=[{"type": "text", "text": "Looks good.\n\nVERDICT: PASS"}],
                        usage_metadata={"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
                    ),
                    {},
                ),
            )

    captured: dict[str, Any] = {}

    def fake_create_cli_agent(model: Any, assistant_id: Any, **kwargs: Any) -> tuple[Any, Any]:
        captured["model"] = model
        captured["assistant_id"] = assistant_id
        captured["kwargs"] = kwargs
        return _FakeAgentGraph(), "fake-backend"

    monkeypatch.setattr(dac, "create_cli_agent", fake_create_cli_agent)
    # context_window_tokens defaults on, so build_coder_agent always resolves
    # the model via create_model now -- fake it to a model-like object with a
    # settable `.profile` (never a real credential/network call).
    monkeypatch.setattr(
        dac, "create_model", lambda spec, **kw: SimpleNamespace(model=SimpleNamespace(profile=None))
    )

    runner = _base_runner()
    graph = reviewer.build_reviewer_graph(run_command=runner)

    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-10",
        "run_id": "run-1",
        "thread_id": "thread-10",
        "workspace": _worktree(tmp_path),
        "issue_text": "Build the thing.",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-10"}}))

    assert order == ["load_context", "ensure_worktree", "load_diff", "run_DAC", "classify", "emit_result"]
    assert result.outcome == Outcome.done
    assert result.tokens.in_ == 7
    assert result.tokens.out == 3

    assert captured["assistant_id"] == "clipse-reviewer"
    # Default-mode routing enforced inside dac.build_coder_agent must never
    # be bypassed just because reviewer.py is doing the calling.
    # get_reviewer_profile() (no override) defaults to shell_allow_list=None
    # (decision 2026-07-07), which build_coder_agent maps to DAC's
    # auto_approve=True/no allow-list.
    assert captured["kwargs"]["auto_approve"] is True
    assert captured["kwargs"]["interrupt_shell_only"] is False
    assert captured["kwargs"]["enable_ask_user"] is True
    assert not captured["kwargs"]["shell_allow_list"]


def test_run_dac_forwards_profile_and_checkpointer(tmp_path):
    agent_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)
    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory(agent_calls),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=_base_runner(),
    )
    workspace = _worktree(tmp_path)
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-11",
        "run_id": "run-1",
        "thread_id": "thread-11",
        "workspace": workspace,
        "issue_text": "x",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-11"}}))

    assert len(agent_calls) == 1
    assert agent_calls[0]["checkpointer"] is None
    assert agent_calls[0]["cwd"] == str(Path(workspace).resolve())
    assert agent_calls[0]["profile"].assistant_id == "clipse-reviewer"


# ---------------------------------------------------------------------------
# Cross-lane checkpoint-thread safety: the Reviewer's own inner DAC thread
# must never collide with the Coder's, even when both lanes' runs share one
# physical checkpoint DB and the same outer thread_id for a given issue
# (see graphs/coder.py's _DAC_THREAD_NAMESPACE_SUFFIX docstring for why the
# collision matters: two structurally different graphs' DAC agents sharing
# one raw thread_id would have the Reviewer's agent resume the Coder's
# entire message history under the Reviewer's own, different system
# prompt).
# ---------------------------------------------------------------------------


def test_run_dac_uses_a_reviewer_specific_dac_thread_namespace_distinct_from_coder(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)
    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=_base_runner(),
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-5",
        "run_id": "run-1",
        "thread_id": "shared-thread",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "shared-thread"}}))

    dac_thread_id = turn_calls[0]["config"]["configurable"]["thread_id"]
    assert dac_thread_id == "shared-thread::review-dac"
    # Compare against graphs.coder's *actual* suffix (not a hardcoded copy of
    # it), so this guard can't silently go stale if Coder's own suffix ever
    # changes.
    coder_dac_thread_id = f"shared-thread{coder._DAC_THREAD_NAMESPACE_SUFFIX}"
    assert dac_thread_id != coder_dac_thread_id


# ---------------------------------------------------------------------------
# ensure_worktree validation (reused from graphs.coder; smoke-tested here to
# prove it is actually wired into this graph)
# ---------------------------------------------------------------------------


def test_ensure_worktree_raises_when_workspace_missing(tmp_path):
    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(
            DacTurnResult(outcome_hint="completed", final_text="", tokens_in=0, tokens_out=0), []
        ),
        run_command=_base_runner(),
    )
    input_state: reviewer.ReviewerState = {
        "issue_id": "SPAC-8",
        "run_id": "run-1",
        "thread_id": "thread-8",
        "workspace": str(tmp_path / "does-not-exist"),
        "issue_text": "x",
    }
    with pytest.raises(reviewer.ReviewerGraphError, match="does not exist"):
        asyncio.run(graph.ainvoke(input_state, {"configurable": {"thread_id": "thread-8"}}))


# ---------------------------------------------------------------------------
# classify (pure)
# ---------------------------------------------------------------------------


def test_classify_pass_verdict():
    out = reviewer.classify({"dac_summary": "Looks correct.\n\nVERDICT: PASS"})
    assert out["review_passed"] is True
    assert out["review_comments"] == []


def test_classify_changes_requested_parses_inline_comments():
    text = (
        "VERDICT: CHANGES_REQUESTED\n"
        "- a/b.py:5: fix this\n"
        "some unrelated prose line\n"
        "- badline-without-colon\n"
        "- c/d.py:9: also fix this\n"
    )
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is False
    assert [c.path for c in out["review_comments"]] == ["a/b.py", "c/d.py"]
    assert [c.line for c in out["review_comments"]] == [5, 9]
    assert [c.body for c in out["review_comments"]] == ["fix this", "also fix this"]


def test_classify_missing_verdict_is_conservative_changes_requested():
    out = reviewer.classify({"dac_summary": "I looked at the diff but forgot to give a verdict."})
    assert out["review_passed"] is False
    assert out["review_comments"] == []


def test_classify_does_not_misread_passing_as_pass():
    # The unanchored regex matched "PASS" as a prefix of "PASSING", so
    # "VERDICT: PASSING" was misread as an actual PASS. A word-boundary
    # anchor must reject it and fall back to the conservative
    # changes_requested default (no verdict recognized).
    out = reviewer.classify({"dac_summary": "Notes.\n\nVERDICT: PASSING for now, will revisit."})
    assert out["review_passed"] is False
    assert out["review_comments"] == []


def test_classify_ignores_bullets_that_appear_before_the_verdict_line():
    text = "- notes/file.py:1: not a real finding, just brainstorming\n\nVERDICT: PASS"
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is True
    assert out["review_comments"] == []


def test_classify_last_verdict_line_wins():
    # A model that quotes the protocol back before its real decision must
    # not be misread from the earlier, quoted occurrence.
    text = "Reminder to self: end with VERDICT: PASS or VERDICT: CHANGES_REQUESTED.\n\nVERDICT: CHANGES_REQUESTED\n- a.py:1: needs work"
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is False
    assert len(out["review_comments"]) == 1


def test_classify_tolerates_empty_dac_summary():
    out = reviewer.classify({})
    assert out["review_passed"] is False
    assert out["review_comments"] == []


# ---------------------------------------------------------------------------
# Task 1: classify reads only the final message (dac_last_text), anchored to
# line starts, with a blocking-finding veto -- guards against a quoted or
# mid-line "VERDICT: PASS" flipping the review.
# ---------------------------------------------------------------------------


def test_classify_prefers_last_text_over_summary() -> None:
    state: reviewer.ReviewerState = {
        "dac_summary": "narration...\nVERDICT: PASS\nmore narration",
        "dac_last_text": "final message\nVERDICT: CHANGES_REQUESTED\n- calc.py:3: blocking: off-by-one",
    }
    out = reviewer.classify(state)
    assert out["review_passed"] is False
    assert out["review_comments"][0].path == "calc.py"


def test_classify_ignores_mid_line_verdict_quote() -> None:
    # A finding body quoting "VERDICT: PASS" after the real verdict must not flip it.
    state: reviewer.ReviewerState = {
        "dac_last_text": (
            "VERDICT: CHANGES_REQUESTED\n"
            "- blocking: calc.py:3: the test asserts VERDICT: PASS is printed, but it never is\n"
        ),
    }
    out = reviewer.classify(state)
    assert out["review_passed"] is False
    assert len(out["review_comments"]) == 1


def test_classify_blocking_findings_veto_pass() -> None:
    state: reviewer.ReviewerState = {
        "dac_last_text": "VERDICT: PASS\n- blocking: calc.py:3: this is actually broken\n",
    }
    out = reviewer.classify(state)
    assert out["review_passed"] is False


def test_classify_pass_with_only_nits_still_passes() -> None:
    # Bullet uses the protocol's real prefix position (`- nit: path:line:
    # body`, per profiles/reviewer.py's system prompt), not `path:line: nit:`
    # -- the severity prefix must precede the path for _INLINE_COMMENT_RE to
    # recognize it as "nit" rather than defaulting to "blocking".
    state: reviewer.ReviewerState = {
        "dac_last_text": "VERDICT: PASS\n- nit: calc.py:3: rename for clarity\n",
    }
    out = reviewer.classify(state)
    assert out["review_passed"] is True


def test_run_dac_stashes_last_text() -> None:
    turn = DacTurnResult(
        outcome_hint="completed",
        final_text="all narration VERDICT: PASS",
        tokens_in=1,
        tokens_out=1,
        last_text="VERDICT: CHANGES_REQUESTED\n- a.py:1: blocking: x",
    )
    calls: list[dict[str, Any]] = []
    node = reviewer.make_run_dac(
        get_reviewer_profile(), _fake_agent_factory(calls), _fake_turn_driver(turn, calls), None
    )
    out = asyncio.run(node({"thread_id": "t", "cwd": "/tmp", "task_text": "review"}))
    assert out["dac_last_text"] == turn.last_text


# ---------------------------------------------------------------------------
# Severity protocol (Task 14): only `blocking:` findings force a rework cycle;
# `nit:` findings still post as comments but never flip the verdict.
# ---------------------------------------------------------------------------


def test_severity_nit_prefixed_finding_parses_with_nit_severity():
    out = reviewer.classify(
        {"dac_summary": "VERDICT: CHANGES_REQUESTED\n- nit: app/x.pbxproj:12: tab width\n"}
    )
    assert [c.severity for c in out["review_comments"]] == ["nit"]
    assert out["review_comments"][0].path == "app/x.pbxproj"
    assert out["review_comments"][0].line == 12
    assert out["review_comments"][0].body == "tab width"


def test_severity_blocking_prefixed_finding_parses_with_blocking_severity():
    out = reviewer.classify(
        {"dac_summary": "VERDICT: CHANGES_REQUESTED\n- blocking: src/a.py:3: null deref\n"}
    )
    assert [c.severity for c in out["review_comments"]] == ["blocking"]
    assert out["review_comments"][0].body == "null deref"


def test_severity_only_nits_pass_despite_changes_requested_verdict():
    # Even a CHANGES_REQUESTED verdict passes when every finding is a nit --
    # nits post as comments but must never trigger a rework cycle.
    text = (
        "VERDICT: CHANGES_REQUESTED\n"
        "- nit: app/x.pbxproj:12: tab width\n"
        "- nit: app/y.swift:4: trailing whitespace\n"
    )
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is True
    assert len(out["review_comments"]) == 2


def test_severity_one_blocking_among_nits_still_blocks():
    text = (
        "VERDICT: CHANGES_REQUESTED\n"
        "- nit: app/x.pbxproj:12: tab width\n"
        "- blocking: src/a.py:3: null deref\n"
        "- nit: app/y.swift:4: trailing whitespace\n"
    )
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is False
    assert len(out["review_comments"]) == 3


def test_severity_unprefixed_finding_still_blocks():
    # Back-compat: an unprefixed finding parses as blocking (conservative).
    text = "VERDICT: CHANGES_REQUESTED\n- src/a.py:3: null deref\n"
    out = reviewer.classify({"dac_summary": text})
    assert [c.severity for c in out["review_comments"]] == ["blocking"]
    assert out["review_passed"] is False


def test_severity_passed_verdict_still_parses_nit_comments():
    # A PASS verdict with nit findings still surfaces them as comments so
    # they land on the PR (route sends them to post_comments).
    text = "VERDICT: PASS\n- nit: app/x.pbxproj:12: tab width\n"
    out = reviewer.classify({"dac_summary": text})
    assert out["review_passed"] is True
    assert len(out["review_comments"]) == 1


def test_severity_passed_with_comments_routes_to_post_comments():
    # Passed-with-comments must still visit post_comments, not skip straight
    # to emit_result, so the nits get posted.
    state = {
        "review_passed": True,
        "review_comments": [reviewer.InlineComment(path="a.py", line=1, body="nit", severity="nit")],
    }
    assert reviewer.route_after_classify(state) == "post_comments"


def test_severity_bare_changes_requested_with_no_parseable_findings_still_blocks():
    # SAFETY-CRITICAL: an explicit CHANGES_REQUESTED whose findings are only
    # unparseable prose must NOT pass. This is exactly the case that would
    # silently flip to PASS if classify used the plan's literal `not blocking`
    # (no findings -> `not [] == True`) instead of `bool(comments) and not
    # blocking`. The verdict is present, so this is distinct from the
    # missing-verdict fail-safe -- both must land as not-passed.
    out = reviewer.classify({"dac_summary": "VERDICT: CHANGES_REQUESTED\nprose only, no parseable bullets"})
    assert out["review_passed"] is False
    assert out["review_comments"] == []


def test_severity_prefix_is_case_insensitive():
    # `re.IGNORECASE` + `.lower()` normalize any casing to the two canonical
    # severities.
    out = reviewer.classify(
        {
            "dac_summary": (
                "VERDICT: CHANGES_REQUESTED\n"
                "- NIT: app/x.pbxproj:12: tab width\n"
                "- Blocking: src/a.py:3: null deref\n"
            )
        }
    )
    assert [c.severity for c in out["review_comments"]] == ["nit", "blocking"]
    # a Blocking finding (any casing) still forces a rework cycle
    assert out["review_passed"] is False


# ---------------------------------------------------------------------------
# route_after_dac / route_after_classify (pure)
# ---------------------------------------------------------------------------


def test_route_after_dac_proceeds_to_classify_when_completed_cleanly():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": False}
    assert reviewer.route_after_dac(state) == "classify"


def test_route_after_dac_routes_to_emit_result_on_interrupt():
    state = {"interrupt_payload": [{"x": 1}], "token_ceiling_exceeded": False}
    assert reviewer.route_after_dac(state) == "emit_result"


def test_route_after_dac_routes_to_emit_result_on_token_ceiling():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": True}
    assert reviewer.route_after_dac(state) == "emit_result"


def test_route_after_classify_to_emit_result_when_passed():
    assert reviewer.route_after_classify({"review_passed": True}) == "emit_result"


def test_route_after_classify_to_post_comments_when_not_passed():
    assert reviewer.route_after_classify({"review_passed": False}) == "post_comments"


# ---------------------------------------------------------------------------
# emit_result (pure)
# ---------------------------------------------------------------------------


def test_emit_result_done_shape():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 0,
        "tokens_in": 10,
        "tokens_out": 20,
        "review_passed": True,
        "review_comments": [],
        "dac_summary": "looks good",
    }
    out = reviewer.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.lane == Lane.reviewer
    assert result.turn_count == 1
    assert out["prior_summary"] == "looks good"


def test_emit_result_changes_requested_shape():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 1,
        "tokens_in": 1,
        "tokens_out": 1,
        "review_passed": False,
        "review_comments": [reviewer.InlineComment(path="a.py", line=1, body="fix")],
        "dac_summary": "needs work",
        "pr_url": "https://github.com/acme/widgets/pull/1",
    }
    out = reviewer.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.changes_requested
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"
    assert result.turn_count == 2


def test_emit_result_changes_requested_summary_carries_failed_findings() -> None:
    state: reviewer.ReviewerState = {
        "run_id": "r1",
        "issue_id": "i1",
        "thread_id": "t1",
        "dac_summary": "found problems",
        "review_passed": False,
        "review_comments": [
            reviewer.InlineComment(path="a.py", line=3, body="off-by-one"),
            reviewer.InlineComment(path="b.py", line=9, body="unused import", severity="nit"),
        ],
        "failed_comments": [reviewer.InlineComment(path="a.py", line=3, body="off-by-one")],
    }
    out = reviewer.emit_result(state)
    summary = out["result"].summary
    assert "Posted 1 inline comment(s)." in summary
    assert "a.py:3: off-by-one" in summary


def test_emit_result_changes_requested_summary_does_not_overclaim_posted_when_no_pr() -> None:
    # The no-PR path (post_comments' honest early return) sets
    # comments_posted=0 while review_comments (from classify) is still
    # non-empty -- the summary must say "Posted 0", never fall back to
    # len(review_comments) and claim comments that were never posted. This
    # text feeds CLIPSE_REVIEW_FEEDBACK on a later rework turn.
    state: reviewer.ReviewerState = {
        "run_id": "r1",
        "issue_id": "i1",
        "thread_id": "t1",
        "dac_summary": "found problems",
        "review_passed": False,
        "review_comments": [
            reviewer.InlineComment(path="a.py", line=3, body="off-by-one"),
            reviewer.InlineComment(path="b.py", line=9, body="unused import"),
        ],
        "comments_posted": 0,
        "comments_failed": 0,
        "failed_comments": [],
    }
    out = reviewer.emit_result(state)
    summary = out["result"].summary
    assert "Posted 2" not in summary
    assert "Posted 0 inline comment(s)." in summary


def test_emit_result_blocked_needs_input_shape():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 2,
        "tokens_in": 1,
        "tokens_out": 1,
        "interrupt_payload": [{"action": "ask"}],
        "dac_summary": "need a decision",
    }
    out = reviewer.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.needs_input
    assert result.pr_url is None
    assert result.turn_count == 3
    assert result.artifacts == []


def test_emit_result_blocked_capability_shape_takes_priority_over_interrupt():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 0,
        "tokens_in": 900,
        "tokens_out": 200,
        "token_ceiling_exceeded": True,
        "interrupt_payload": [{"action": "ask"}],
    }
    out = reviewer.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability


def test_emit_result_requires_issue_run_and_thread_ids():
    with pytest.raises(KeyError):
        reviewer.emit_result({})


# ---------------------------------------------------------------------------
# Checkpointer-driven state carryover (compiled with a checkpointer)
# ---------------------------------------------------------------------------


def test_checkpointer_scopes_state_by_thread_id(tmp_path):
    async def turn_driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        return DacTurnResult(outcome_hint="completed", final_text="turn output.\n\nVERDICT: PASS", tokens_in=1, tokens_out=1)

    checkpointer = InMemorySaver()
    graph = reviewer.build_reviewer_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=turn_driver,
        run_command=_base_runner(),
        checkpointer=checkpointer,
    )
    workspace = _worktree(tmp_path)
    base_input: reviewer.ReviewerState = {
        "issue_id": "SPAC-7",
        "run_id": "run-1",
        "thread_id": "thread-7",
        "workspace": workspace,
        "issue_text": "Review X.",
    }

    async def drive() -> tuple[WorkerResult, WorkerResult]:
        first = await graph.ainvoke(base_input, {"configurable": {"thread_id": "thread-7"}})
        other_issue = {**base_input, "issue_id": "SPAC-8", "thread_id": "thread-8"}
        second = await graph.ainvoke(other_issue, {"configurable": {"thread_id": "thread-8"}})
        return first["result"], second["result"]

    first_result, second_result = asyncio.run(drive())
    assert first_result.turn_count == 1
    assert second_result.turn_count == 1  # a fresh thread never sees issue-7's history
