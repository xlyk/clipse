"""Tests for the Coder lane's LangGraph graph (`clipse_agent.graphs.coder`).

DAC (`dac.build_coder_agent` / `dac.drive_turn`) and git/gh are always faked
here via the graph's own dependency-injection seams (`agent_factory`,
`turn_driver`, `run_command`) -- these tests never touch a real model, a
real DAC agent, or a real subprocess/network call. `run_DAC` is the graph's
only async node, so the compiled graph must be driven with `.ainvoke`/
`.astream`; per `test_dac.py`'s convention, plain `asyncio.run` drives it
(no pytest-asyncio in this repo's approved dev deps).
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
from clipse_agent.graphs import coder

# ---------------------------------------------------------------------------
# Fakes / fixtures
# ---------------------------------------------------------------------------


def _worktree(tmp_path: Path) -> str:
    """Build a fake worktree dir: just a directory with a `.git` marker.

    `ensure_worktree` only ever checks for existence + a `.git` entry -- it
    never shells out to `git` to verify a real repo -- so no real git repo
    is needed here.
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


# `sync_base` (Task 3) and `make_commit` (Task 3) both probe MERGE_HEAD via
# `git rev-parse -q --verify MERGE_HEAD`. Every fixture/test below that
# doesn't care about an in-progress merge needs this to report "no merge in
# progress" (non-zero exit) -- otherwise `FakeRunner`'s default (a clean
# zero-exit, empty-stdout success for anything unscripted) would make every
# such probe look like a merge IS in progress, silently rerouting sync_base
# away from its fetch/merge path.
_NO_MERGE_IN_PROGRESS = (
    _starts_with("git", "rev-parse", "-q", "--verify", "MERGE_HEAD"),
    coder.CommandResult(1, "", "fatal: Needed a single revision"),
)


class FakeRunner:
    """Injectable stand-in for `coder.CommandRunner`.

    Matches each call against `rules` (checked in order); the first
    matching predicate wins. A call matching nothing gets `default` -- a
    clean no-output success -- so a test only has to script the calls it
    actually cares about (e.g. never `git add -A`). Every call is recorded
    in `calls`, in order, so a test can assert on exactly what ran.
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
    pr_exists: bool = False,
    pr_url: str = "https://github.com/acme/widgets/pull/1",
    changed_files: str = " M src/thing.py\n",
) -> FakeRunner:
    view_result = (
        coder.CommandResult(0, json.dumps({"url": pr_url}), "")
        if pr_exists
        else coder.CommandResult(1, "", "no pull requests found for branch")
    )
    return FakeRunner(
        rules=[
            (
                _starts_with("git", "rev-parse", "--abbrev-ref", "HEAD"),
                coder.CommandResult(0, f"{branch}\n", ""),
            ),
            _NO_MERGE_IN_PROGRESS,
            (_starts_with("git", "status", "--porcelain"), coder.CommandResult(0, changed_files, "")),
            (_starts_with("gh", "pr", "view"), view_result),
            (_starts_with("gh", "pr", "create"), coder.CommandResult(0, f"{pr_url}\n", "")),
        ],
    )


def _fake_agent_factory(calls: list[dict[str, Any]]) -> Callable[..., tuple[str, str]]:
    def factory(profile: Any, checkpointer: Any, cwd: str) -> tuple[str, str]:
        calls.append({"profile": profile, "checkpointer": checkpointer, "cwd": cwd})
        return "fake-agent-graph", "fake-backend"

    return factory


def _fake_turn_driver(
    result: DacTurnResult, calls: list[dict[str, Any]]
) -> Callable[..., Any]:
    async def driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        calls.append({"agent_graph": agent_graph, "config": config, **kwargs})
        return result

    return driver


def _is_docs_turn(config: Any) -> bool:
    """True iff `config` targets the documentation DAC turn (its own thread
    namespace) rather than the coding turn -- see coder._DOCS_DAC_THREAD_NAMESPACE_SUFFIX."""
    return config["configurable"]["thread_id"].endswith(coder._DOCS_DAC_THREAD_NAMESPACE_SUFFIX)


def _fake_turn_driver_by_turn(
    code_result: DacTurnResult, docs_result: DacTurnResult, calls: list[dict[str, Any]]
) -> Callable[..., Any]:
    """A turn driver returning distinct results for the coding turn vs the
    documentation turn, keyed off the DAC thread namespace, so a test can
    assert the two turns' tokens/task_text/config independently. Both graph
    nodes (`run_DAC`, `run_docs`) share one injected driver."""

    async def driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        calls.append({"agent_graph": agent_graph, "config": config, **kwargs})
        return docs_result if _is_docs_turn(config) else code_result

    return driver


async def _drive(
    graph: Any, input_state: dict[str, Any], config: dict[str, Any]
) -> tuple[list[str], WorkerResult]:
    """Run `graph` once via `.astream(..., stream_mode="updates")`.

    Returns (node execution order, the WorkerResult `emit_result`
    produced). Only `emit_result` ever writes the `result` key, so
    grabbing it off that node's own update -- rather than invoking the
    graph a second time to read the merged final state -- keeps a
    stateful/checkpointed graph from being driven twice per test.
    """
    order: list[str] = []
    result: WorkerResult | None = None
    async for update in graph.astream(input_state, config, stream_mode="updates"):
        node, partial = next(iter(update.items()))
        order.append(node)
        # A node returning no updates (e.g. `push`'s `{}`) surfaces here as
        # None, not an empty dict.
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
# Happy path: full node order, needs_review result
# ---------------------------------------------------------------------------


def test_happy_path_runs_full_node_order_and_emits_needs_review(tmp_path):
    runner = _base_runner()
    agent_calls: list[dict[str, Any]] = []
    turn_calls: list[dict[str, Any]] = []
    code_result = DacTurnResult(
        outcome_hint="completed",
        final_text="Implemented the widget factory.",
        tokens_in=120,
        tokens_out=340,
    )
    docs_result = DacTurnResult(
        outcome_hint="completed",
        final_text="Documented the widget factory.",
        tokens_in=5,
        tokens_out=7,
    )

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory(agent_calls),
        turn_driver=_fake_turn_driver_by_turn(code_result, docs_result, turn_calls),
        run_command=runner,
    )

    workspace = _worktree(tmp_path)
    input_state: coder.CoderState = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "workspace": workspace,
        "issue_text": "Build the widget factory.",
    }
    config = {"configurable": {"thread_id": "thread-1"}}

    order, result = asyncio.run(_drive(graph, input_state, config))

    assert order == [
        "load_context",
        "ensure_worktree",
        "sync_base",
        "run_DAC",
        "run_docs",
        "commit",
        "push",
        "open_PR",
        "emit_result",
    ]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.lane == Lane.coder
    assert result.run_id == "run-1"
    assert result.issue_id == "SPAC-1"
    assert result.thread_id == "thread-1"
    assert result.turn_count == 1
    # Tokens sum the coding turn (120/340) and the documentation turn (5/7).
    assert result.tokens.in_ == 125
    assert result.tokens.out == 347
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"
    assert result.artifacts == ["src/thing.py"]

    # Two DAC turns ran: the coding turn (::dac, issue text) then the docs turn
    # (::docs-dac, a docs prompt), both fresh task_text (no resume).
    assert len(turn_calls) == 2
    assert turn_calls[0]["task_text"] == "Build the widget factory."
    assert turn_calls[0]["config"]["configurable"]["thread_id"].endswith(coder._DAC_THREAD_NAMESPACE_SUFFIX)
    assert "resume" not in turn_calls[0]
    assert _is_docs_turn(turn_calls[1]["config"])
    assert turn_calls[1]["config"]["configurable"]["thread_id"] != turn_calls[0]["config"]["configurable"]["thread_id"]
    assert "resume" not in turn_calls[1]
    assert "Build the widget factory." in turn_calls[1]["task_text"]  # issue text folded into the docs prompt
    assert "not committed" in turn_calls[1]["task_text"]

    # Both DAC agents were built from this turn's resolved cwd, no checkpointer;
    # the docs turn uses the restricted docs profile.
    assert len(agent_calls) == 2
    assert all(c["checkpointer"] is None and c["cwd"] == str(Path(workspace).resolve()) for c in agent_calls)
    assert agent_calls[0]["profile"].assistant_id == "clipse-coder"
    assert agent_calls[1]["profile"].assistant_id == "clipse-coder-docs"


# ---------------------------------------------------------------------------
# Documentation node: best-effort (never blocks the PR), clean-path only
# ---------------------------------------------------------------------------


def _needs_review_input(tmp_path: Path, thread: str = "thread-docs") -> coder.CoderState:
    return {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": thread,
        "workspace": _worktree(tmp_path),
        "issue_text": "Build the widget factory.",
    }


def test_docs_turn_token_ceiling_is_non_blocking(tmp_path):
    # A docs-turn token-ceiling must NOT block the run -- the coding turn
    # already produced a review-ready PR; docs are an enhancement.
    code_result = DacTurnResult(outcome_hint="completed", final_text="did the code", tokens_in=120, tokens_out=340)
    docs_result = DacTurnResult(
        outcome_hint="completed", final_text="", tokens_in=50, tokens_out=0, token_ceiling_exceeded=True
    )
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver_by_turn(code_result, docs_result, []),
        run_command=_base_runner(),
    )

    order, result = asyncio.run(_drive(graph, _needs_review_input(tmp_path), {"configurable": {"thread_id": "thread-docs"}}))

    assert "run_docs" in order and "open_PR" in order
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"
    # Docs tokens still count toward the total even though the turn was cut off.
    assert result.tokens.in_ == 170
    assert result.tokens.out == 340


def test_docs_turn_interrupt_is_non_blocking(tmp_path):
    code_result = DacTurnResult(outcome_hint="completed", final_text="did the code", tokens_in=120, tokens_out=340)
    docs_result = DacTurnResult(
        outcome_hint="interrupted", final_text="", tokens_in=5, tokens_out=5, interrupt_payload=[{"ask": "which format?"}]
    )
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver_by_turn(code_result, docs_result, []),
        run_command=_base_runner(),
    )

    _order, result = asyncio.run(_drive(graph, _needs_review_input(tmp_path), {"configurable": {"thread_id": "thread-docs"}}))

    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"


def test_docs_turn_exception_is_swallowed(tmp_path):
    code_result = DacTurnResult(outcome_hint="completed", final_text="did the code", tokens_in=120, tokens_out=340)

    async def driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        if _is_docs_turn(config):
            raise RuntimeError("docs agent blew up")
        return code_result

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=driver,
        run_command=_base_runner(),
    )

    order, result = asyncio.run(_drive(graph, _needs_review_input(tmp_path), {"configurable": {"thread_id": "thread-docs"}}))

    assert "run_docs" in order and "open_PR" in order
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    # A swallowed docs failure contributes zero tokens: only the coding turn counts.
    assert result.tokens.in_ == 120
    assert result.tokens.out == 340


def test_code_turn_interrupt_skips_docs_entirely(tmp_path):
    # When the CODING turn blocks (interrupt), docs never run and the result is blocked.
    blocked_code = DacTurnResult(
        outcome_hint="interrupted", final_text="need a decision", tokens_in=10, tokens_out=5,
        interrupt_payload=[{"ask": "x"}],
    )
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(blocked_code, []),
        run_command=_base_runner(),
    )

    order, result = asyncio.run(_drive(graph, _needs_review_input(tmp_path), {"configurable": {"thread_id": "thread-docs"}}))

    assert order == ["load_context", "ensure_worktree", "sync_base", "run_DAC", "emit_result"]
    assert "run_docs" not in order
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.needs_input


def test_docs_no_op_still_needs_review(tmp_path):
    # Both turns leave no file changes: commit is skipped, but the PR is still
    # (re)opened and the outcome is needs_review -- a docs no-op is expected.
    clean = DacTurnResult(outcome_hint="completed", final_text="looked around, nothing to change", tokens_in=1, tokens_out=1)
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(clean, []),
        run_command=_base_runner(changed_files=""),
    )

    order, result = asyncio.run(_drive(graph, _needs_review_input(tmp_path), {"configurable": {"thread_id": "thread-docs"}}))

    assert "run_docs" in order
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.artifacts == []


# ---------------------------------------------------------------------------
# Idempotent open_PR
# ---------------------------------------------------------------------------


def test_open_pr_reuses_existing_pr_when_gh_pr_view_succeeds(tmp_path):
    runner = _base_runner(pr_exists=True, pr_url="https://github.com/acme/widgets/pull/7")
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-2",
        "run_id": "run-1",
        "thread_id": "thread-2",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    _, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-2"}}))

    assert result.pr_url == "https://github.com/acme/widgets/pull/7"
    view_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "view"]]
    create_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "create"]]
    assert len(view_calls) == 1
    assert create_calls == []


def test_open_pr_creates_when_gh_pr_view_finds_nothing(tmp_path):
    runner = _base_runner(pr_exists=False, pr_url="https://github.com/acme/widgets/pull/9")
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-3",
        "run_id": "run-1",
        "thread_id": "thread-3",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    _, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-3"}}))

    assert result.pr_url == "https://github.com/acme/widgets/pull/9"
    create_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "create"]]
    assert len(create_calls) == 1
    assert "--head" in create_calls[0].argv
    assert "clipse/spac-1" in create_calls[0].argv
    assert "--base" in create_calls[0].argv
    # Coder-authored PRs always open as drafts -- the Coder lane's own turn
    # ending doesn't mean the work is reviewed/ready; only the Reviewer lane
    # should take a PR out of draft.
    assert "--draft" in create_calls[0].argv


# ---------------------------------------------------------------------------
# Interrupt -> blocked(needs_input)
# ---------------------------------------------------------------------------


def test_interrupt_emits_blocked_needs_input_and_skips_git_and_pr(tmp_path):
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="interrupted",
        final_text="paused, needs a decision",
        tokens_in=10,
        tokens_out=5,
        interrupt_payload=[{"action_requests": [{"name": "shell", "args": {"command": "rm -rf /"}}]}],
    )

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-4",
        "run_id": "run-1",
        "thread_id": "thread-4",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-4"}}))

    assert order == ["load_context", "ensure_worktree", "sync_base", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.needs_input
    assert result.pr_url is None
    assert result.tokens.in_ == 10
    assert result.tokens.out == 5

    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "commit") not in verbs
    assert ("git", "push") not in verbs
    assert ("gh", "pr") not in verbs


def test_token_ceiling_exceeded_emits_blocked_capability_even_if_interrupted(tmp_path):
    # token_ceiling_exceeded must win over interrupt_payload (dac.py's own
    # contract: "token_ceiling_exceeded maps to blocked/capability
    # regardless of outcome_hint").
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="interrupted",
        final_text="ran out of budget",
        tokens_in=900,
        tokens_out=200,
        interrupt_payload=[{"some": "payload"}],
        token_ceiling_exceeded=True,
    )
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-5",
        "run_id": "run-1",
        "thread_id": "thread-5",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "max_tokens": 1000,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-5"}}))

    assert order == ["load_context", "ensure_worktree", "sync_base", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability
    assert "1100" in result.summary


# ---------------------------------------------------------------------------
# Resume (continuation after a prior interrupt)
# ---------------------------------------------------------------------------


def test_resume_turn_drives_dac_with_resume_payload_not_task_text(tmp_path):
    runner = _base_runner()
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="resumed and finished", tokens_in=5, tokens_out=5)

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-6",
        "run_id": "run-2",
        "thread_id": "thread-6",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "resume_payload": {"int-1": {"decisions": [{"type": "approve"}]}},
        "turn_count": 1,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-6"}}))

    assert order[:4] == ["load_context", "ensure_worktree", "sync_base", "run_DAC"]
    assert turn_calls[0]["resume"] == {"int-1": {"decisions": [{"type": "approve"}]}}
    assert "task_text" not in turn_calls[0]
    assert result.turn_count == 2


# ---------------------------------------------------------------------------
# Checkpointer-driven state carryover ("compiled with the checkpointer for
# resume")
# ---------------------------------------------------------------------------


def test_checkpointer_carries_dac_summary_forward_as_next_turns_prior_summary(tmp_path):
    produced_summaries = iter(["did the first part", "did the second part"])
    captured_task_texts: list[str] = []

    async def turn_driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        # The docs turn must not consume the coding-summary sequence (nor be
        # captured as a coding task_text); it runs each turn but is irrelevant here.
        if _is_docs_turn(config):
            return DacTurnResult(outcome_hint="completed", final_text="docs turn", tokens_in=0, tokens_out=0)
        captured_task_texts.append(kwargs.get("task_text", ""))
        return DacTurnResult(outcome_hint="completed", final_text=next(produced_summaries), tokens_in=1, tokens_out=1)

    runner = _base_runner()
    checkpointer = InMemorySaver()
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=turn_driver,
        run_command=runner,
        checkpointer=checkpointer,
    )
    config = {"configurable": {"thread_id": "thread-7"}}
    workspace = _worktree(tmp_path)
    base_input: coder.CoderState = {
        "issue_id": "SPAC-7",
        "run_id": "run-1",
        "thread_id": "thread-7",
        "workspace": workspace,
        "issue_text": "Build X.",
    }

    async def drive_twice() -> None:
        await graph.ainvoke(base_input, config)
        # Second turn: the caller doesn't re-supply prior_summary -- it
        # must still be there from the first turn's checkpointed state.
        await graph.ainvoke({**base_input, "run_id": "run-2"}, config)

    asyncio.run(drive_twice())

    assert captured_task_texts[0] == "Build X."
    assert "did the first part" in captured_task_texts[1]


def test_checkpointer_scopes_state_by_thread_id(tmp_path):
    async def turn_driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        return DacTurnResult(outcome_hint="completed", final_text="turn output", tokens_in=1, tokens_out=1)

    runner = _base_runner()
    checkpointer = InMemorySaver()
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=turn_driver,
        run_command=runner,
        checkpointer=checkpointer,
    )
    workspace = _worktree(tmp_path)
    base_input: coder.CoderState = {
        "issue_id": "SPAC-7",
        "run_id": "run-1",
        "thread_id": "thread-7",
        "workspace": workspace,
        "issue_text": "Build X.",
    }

    async def drive() -> tuple[WorkerResult, WorkerResult]:
        first = await graph.ainvoke(base_input, {"configurable": {"thread_id": "thread-7"}})
        other_issue = {**base_input, "issue_id": "SPAC-8", "thread_id": "thread-8"}
        second = await graph.ainvoke(other_issue, {"configurable": {"thread_id": "thread-8"}})
        return first["result"], second["result"]

    first_result, second_result = asyncio.run(drive())
    assert first_result.turn_count == 1
    assert second_result.turn_count == 1  # a fresh thread never sees issue-7's history


# ---------------------------------------------------------------------------
# ensure_worktree validation
# ---------------------------------------------------------------------------


def test_ensure_worktree_raises_when_workspace_missing(tmp_path):
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(
            DacTurnResult(outcome_hint="completed", final_text="", tokens_in=0, tokens_out=0), []
        ),
        run_command=_base_runner(),
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-8",
        "run_id": "run-1",
        "thread_id": "thread-8",
        "workspace": str(tmp_path / "does-not-exist"),
        "issue_text": "x",
    }
    with pytest.raises(coder.CoderGraphError, match="does not exist"):
        asyncio.run(graph.ainvoke(input_state, {"configurable": {"thread_id": "thread-8"}}))


def test_ensure_worktree_raises_when_not_a_git_dir(tmp_path):
    not_git = tmp_path / "plain-dir"
    not_git.mkdir()
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(
            DacTurnResult(outcome_hint="completed", final_text="", tokens_in=0, tokens_out=0), []
        ),
        run_command=_base_runner(),
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-9",
        "run_id": "run-1",
        "thread_id": "thread-9",
        "workspace": str(not_git),
        "issue_text": "x",
    }
    with pytest.raises(coder.CoderGraphError, match="not a git worktree"):
        asyncio.run(graph.ainvoke(input_state, {"configurable": {"thread_id": "thread-9"}}))


# ---------------------------------------------------------------------------
# sync_base (needs an injected run_command) -- unit-level coverage of every
# branch, mirroring make_ensure_worktree/make_commit's own direct-node-call
# test style rather than driving the full graph for each case.
# ---------------------------------------------------------------------------


def test_sync_base_is_a_no_op_when_base_branch_is_empty(tmp_path):
    runner = FakeRunner()
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path), "base_branch": ""})

    assert out == {"merge_conflict_files": []}
    assert runner.calls == []


def test_sync_base_is_a_no_op_when_base_branch_is_absent(tmp_path):
    runner = FakeRunner()
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path)})

    assert out == {"merge_conflict_files": []}
    assert runner.calls == []


def test_sync_base_clean_merge_returns_no_conflicts(tmp_path):
    runner = FakeRunner(
        rules=[
            _NO_MERGE_IN_PROGRESS,
            (_starts_with("git", "fetch", "origin", "main"), coder.CommandResult(0, "", "")),
            (
                _starts_with("git", "merge", "--no-edit", "origin/main"),
                coder.CommandResult(0, "Already up to date.\n", ""),
            ),
        ]
    )
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": []}
    verbs = [tuple(c.argv) for c in runner.calls]
    assert ("git", "fetch", "origin", "main") in verbs
    assert ("git", "merge", "--no-edit", "origin/main") in verbs
    # A clean merge never probes for conflicts or aborts anything.
    assert not any(c.argv[:2] == ["git", "diff"] for c in runner.calls)
    assert ("git", "merge", "--abort") not in verbs


def test_sync_base_conflict_parses_unmerged_files_and_leaves_merge_in_progress(tmp_path):
    runner = FakeRunner(
        rules=[
            _NO_MERGE_IN_PROGRESS,
            (_starts_with("git", "fetch", "origin", "main"), coder.CommandResult(0, "", "")),
            (
                _starts_with("git", "merge", "--no-edit", "origin/main"),
                coder.CommandResult(1, "", "CONFLICT (content): Merge conflict in src/a.py"),
            ),
            (
                _starts_with("git", "diff", "--name-only", "--diff-filter=U"),
                coder.CommandResult(0, "src/a.py\nsrc/b.py\n", ""),
            ),
        ]
    )
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": ["src/a.py", "src/b.py"]}
    # Left mid-merge on purpose -- the coder resolves it next turn.
    verbs = [tuple(c.argv) for c in runner.calls]
    assert ("git", "merge", "--abort") not in verbs


def test_sync_base_fetch_failure_warns_and_skips_without_attempting_merge(tmp_path, caplog):
    runner = FakeRunner(
        rules=[
            _NO_MERGE_IN_PROGRESS,
            (
                _starts_with("git", "fetch", "origin", "main"),
                coder.CommandResult(128, "", "unable to access 'https://example/repo': Could not resolve host"),
            ),
        ]
    )
    node = coder.make_sync_base(runner)

    with caplog.at_level("WARNING"):
        out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": []}
    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "merge") not in verbs
    assert ("git", "diff") not in verbs
    assert "fetch" in caplog.text


def test_sync_base_unexpected_merge_error_without_conflicts_aborts_and_proceeds(tmp_path, caplog):
    runner = FakeRunner(
        rules=[
            _NO_MERGE_IN_PROGRESS,
            (_starts_with("git", "fetch", "origin", "main"), coder.CommandResult(0, "", "")),
            (
                _starts_with("git", "merge", "--no-edit", "origin/main"),
                coder.CommandResult(128, "", "fatal: not something we can merge"),
            ),
            (_starts_with("git", "diff", "--name-only", "--diff-filter=U"), coder.CommandResult(0, "", "")),
        ]
    )
    node = coder.make_sync_base(runner)

    with caplog.at_level("WARNING"):
        out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": []}
    verbs = [tuple(c.argv) for c in runner.calls]
    assert ("git", "merge", "--abort") in verbs
    assert "merge" in caplog.text


def test_sync_base_merge_head_guard_resumes_without_starting_new_merge(tmp_path):
    # A prior turn's conflict-resolution DAC call was interrupted (or hit its
    # token ceiling) before `make_commit` could complete the merge, leaving
    # MERGE_HEAD set. This turn must NOT attempt a second merge (git refuses
    # that outright) and must NOT abort the merge it didn't start -- it just
    # resumes by re-deriving the still-unresolved files from the index.
    runner = FakeRunner(
        rules=[
            (_starts_with("git", "rev-parse", "-q", "--verify", "MERGE_HEAD"), coder.CommandResult(0, "", "")),
            (
                _starts_with("git", "diff", "--name-only", "--diff-filter=U"),
                coder.CommandResult(0, "src/a.py\nsrc/b.py\n", ""),
            ),
        ]
    )
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": ["src/a.py", "src/b.py"]}
    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "fetch") not in verbs
    assert ("git", "merge") not in verbs


def test_sync_base_merge_head_guard_can_resume_to_no_remaining_conflicts(tmp_path):
    # A prior turn resolved AND staged the files (git add) but crashed before
    # `git commit --no-edit` concluded the merge: the index has no more
    # unmerged entries, so re-deriving yields an empty list. This is a valid
    # outcome -- `_coding_task_text` falls back to the normal issue task, and
    # `make_commit`'s own `git status --porcelain` still picks up the staged
    # change on this turn.
    runner = FakeRunner(
        rules=[
            (_starts_with("git", "rev-parse", "-q", "--verify", "MERGE_HEAD"), coder.CommandResult(0, "", "")),
            (_starts_with("git", "diff", "--name-only", "--diff-filter=U"), coder.CommandResult(0, "", "")),
        ]
    )
    node = coder.make_sync_base(runner)

    out = node({"cwd": str(tmp_path), "base_branch": "main"})

    assert out == {"merge_conflict_files": []}
    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "fetch") not in verbs
    assert ("git", "merge") not in verbs


def test_sync_base_wired_between_ensure_worktree_and_run_dac(tmp_path):
    # Graph-level: proves the node is actually wired into the edge sequence
    # (ensure_worktree -> sync_base -> run_DAC) and a clean sync doesn't
    # stop the turn from reaching run_DAC and completing normally.
    runner = _base_runner()
    turn_result = DacTurnResult(outcome_hint="completed", final_text="did the work", tokens_in=1, tokens_out=1)
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-11",
        "run_id": "run-1",
        "thread_id": "thread-11",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "base_branch": "main",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-11"}}))

    assert order == [
        "load_context",
        "ensure_worktree",
        "sync_base",
        "run_DAC",
        "run_docs",
        "commit",
        "push",
        "open_PR",
        "emit_result",
    ]
    _assert_valid_result(result, blocked=False)
    fetch_calls = [c for c in runner.calls if c.argv[:2] == ["git", "fetch"]]
    merge_calls = [c for c in runner.calls if c.argv[:2] == ["git", "merge"]]
    assert fetch_calls and fetch_calls[0].argv == ["git", "fetch", "origin", "main"]
    assert merge_calls and merge_calls[0].argv == ["git", "merge", "--no-edit", "origin/main"]


# ---------------------------------------------------------------------------
# run_DAC's task text: conflict-resolution turn vs the normal issue/rework
# task, branching on `merge_conflict_files` (`_coding_task_text`, pure) --
# mirrors `_docs_task_text`'s own pure-function + wired-node coverage.
# ---------------------------------------------------------------------------


def test_coding_task_text_uses_normal_task_when_no_conflict_files_key():
    state = {"task_text": "Build the widget factory."}
    assert coder._coding_task_text(state) == "Build the widget factory."


def test_coding_task_text_uses_normal_task_when_conflict_files_is_empty_list():
    state = {"task_text": "Build the widget factory.", "merge_conflict_files": []}
    assert coder._coding_task_text(state) == "Build the widget factory."


def test_coding_task_text_names_files_and_markers_when_conflicts_present():
    state = {
        "task_text": "Build the widget factory.",
        "merge_conflict_files": ["src/a.py", "src/b.py"],
    }

    task = coder._coding_task_text(state)

    assert "src/a.py" in task
    assert "src/b.py" in task
    assert "<<<<<<<" in task
    assert "=======" in task
    assert ">>>>>>>" in task
    assert "both" in task.lower()  # preserve BOTH sides' intent
    # This turn REPLACES the normal issue task -- it never rides along.
    assert "Build the widget factory." not in task


def test_run_dac_sends_conflict_resolution_task_to_turn_driver(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="resolved", tokens_in=1, tokens_out=1)
    node = coder.make_run_dac(
        "unused-profile",
        _fake_agent_factory([]),
        _fake_turn_driver(turn_result, turn_calls),
        None,
    )
    state: coder.CoderState = {
        "cwd": str(tmp_path),
        "thread_id": "thread-conflict",
        "task_text": "Build the widget factory.",
        "merge_conflict_files": ["src/a.py", "src/b.py"],
    }

    asyncio.run(node(state))

    sent = turn_calls[0]["task_text"]
    assert "src/a.py" in sent
    assert "src/b.py" in sent
    assert "<<<<<<<" in sent
    assert "Build the widget factory." not in sent


def test_run_dac_sends_normal_task_text_when_no_merge_conflict_files(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)
    node = coder.make_run_dac(
        "unused-profile",
        _fake_agent_factory([]),
        _fake_turn_driver(turn_result, turn_calls),
        None,
    )
    state: coder.CoderState = {
        "cwd": str(tmp_path),
        "thread_id": "thread-normal",
        "task_text": "Build the widget factory.",
    }

    asyncio.run(node(state))

    assert turn_calls[0]["task_text"] == "Build the widget factory."


def test_conflict_resolution_turn_completes_merge_and_reaches_needs_review(tmp_path):
    # Full graph, end to end: sync_base hits a real conflict; run_DAC gets a
    # conflict-resolution task (never the normal issue task); the turn
    # completes cleanly; commit concludes the merge with `--no-edit`; push
    # and open_PR run unchanged, landing on needs_review.
    runner = FakeRunner(
        rules=[
            _NO_MERGE_IN_PROGRESS,
            (
                _starts_with("git", "rev-parse", "--abbrev-ref", "HEAD"),
                coder.CommandResult(0, "clipse/spac-12\n", ""),
            ),
            (_starts_with("git", "fetch", "origin", "main"), coder.CommandResult(0, "", "")),
            (
                _starts_with("git", "merge", "--no-edit", "origin/main"),
                coder.CommandResult(1, "", "CONFLICT (content): Merge conflict in src/a.py"),
            ),
            (
                _starts_with("git", "diff", "--name-only", "--diff-filter=U"),
                coder.CommandResult(0, "src/a.py\n", ""),
            ),
            (_starts_with("git", "status", "--porcelain"), coder.CommandResult(0, "M  src/a.py\n", "")),
            (_starts_with("gh", "pr", "view"), coder.CommandResult(1, "", "no pull requests found for branch")),
            (
                _starts_with("gh", "pr", "create"),
                coder.CommandResult(0, "https://github.com/acme/widgets/pull/12\n", ""),
            ),
        ]
    )
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(
        outcome_hint="completed", final_text="Resolved the conflict.", tokens_in=8, tokens_out=4
    )

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: coder.CoderState = {
        "issue_id": "SPAC-12",
        "run_id": "run-1",
        "thread_id": "thread-12",
        "workspace": _worktree(tmp_path),
        "issue_text": "Build the widget factory.",
        "base_branch": "main",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-12"}}))

    assert order == [
        "load_context",
        "ensure_worktree",
        "sync_base",
        "run_DAC",
        "run_docs",
        "commit",
        "push",
        "open_PR",
        "emit_result",
    ]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.com/acme/widgets/pull/12"

    # The coding turn's task was conflict resolution, never the issue text.
    code_call = next(c for c in turn_calls if not _is_docs_turn(c["config"]))
    assert "src/a.py" in code_call["task_text"]
    assert "<<<<<<<" in code_call["task_text"]
    assert "Build the widget factory." not in code_call["task_text"]

    commit_calls = [c for c in runner.calls if c.argv[:2] == ["git", "commit"]]
    assert len(commit_calls) == 1
    assert commit_calls[0].argv == ["git", "commit", "--no-edit"]

    push_calls = [c for c in runner.calls if c.argv[:2] == ["git", "push"]]
    assert len(push_calls) == 1
    assert push_calls[0].argv == ["git", "push", "--set-upstream", "origin", "clipse/spac-12"]


# ---------------------------------------------------------------------------
# Default wiring really reaches clipse_agent.dac (only DAC's own innermost
# create_cli_agent/create_model are faked, same seam test_dac.py uses).
# ---------------------------------------------------------------------------


def test_build_coder_graph_default_wiring_uses_real_dac_module(tmp_path, monkeypatch):
    class _FakeAgentGraph:
        async def astream(self, stream_input: Any, **kwargs: Any):
            yield (
                (),
                "messages",
                (
                    AIMessage(
                        content=[{"type": "text", "text": "done"}],
                        usage_metadata={"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
                    ),
                    {},
                ),
            )

    captured: dict[str, Any] = {}

    def fake_create_cli_agent(model: Any, assistant_id: Any, **kwargs: Any) -> tuple[Any, Any]:
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
    graph = coder.build_coder_graph(run_command=runner)

    input_state: coder.CoderState = {
        "issue_id": "SPAC-10",
        "run_id": "run-1",
        "thread_id": "thread-10",
        "workspace": _worktree(tmp_path),
        "issue_text": "Build the thing.",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-10"}}))

    assert order == [
        "load_context",
        "ensure_worktree",
        "sync_base",
        "run_DAC",
        "run_docs",
        "commit",
        "push",
        "open_PR",
        "emit_result",
    ]
    assert result.outcome == Outcome.needs_review
    # The same _FakeAgentGraph (7/3) is streamed for both the coding and docs
    # turns, so the emitted totals are their sum.
    assert result.tokens.in_ == 14
    assert result.tokens.out == 6
    # Safety invariant enforced inside dac.build_coder_agent must never be
    # bypassed just because coder.py is doing the calling.
    assert captured["kwargs"]["auto_approve"] is False
    assert captured["kwargs"]["interrupt_shell_only"] is True
    assert captured["kwargs"]["shell_allow_list"]


# ---------------------------------------------------------------------------
# load_context (pure, no injected deps)
# ---------------------------------------------------------------------------


def test_load_context_uses_issue_text_directly_when_no_prior_summary():
    out = coder.load_context({"issue_text": "Fix the bug."})
    assert out["task_text"] == "Fix the bug."


def test_load_context_folds_prior_summary_into_task_text():
    out = coder.load_context({"issue_text": "Fix the bug.", "prior_summary": "Already found the root cause."})
    assert "Fix the bug." in out["task_text"]
    assert "Already found the root cause." in out["task_text"]


def test_load_context_falls_back_to_env_var_when_issue_text_missing(monkeypatch):
    monkeypatch.setenv("CLIPSE_ISSUE_TEXT", "from env")
    out = coder.load_context({})
    assert out["task_text"] == "from env"


def test_load_context_tolerates_completely_empty_state():
    out = coder.load_context({})
    assert out["task_text"] == ""


def test_load_context_folds_review_feedback_into_task_text():
    out = coder.load_context(
        {"issue_text": "Fix the bug.", "review_feedback": "Remove the fabricated config section."}
    )
    assert "Fix the bug." in out["task_text"]
    assert "The previous review requested these changes" in out["task_text"]
    assert "Remove the fabricated config section." in out["task_text"]


def test_load_context_falls_back_to_review_feedback_env_var(monkeypatch):
    monkeypatch.setenv("CLIPSE_REVIEW_FEEDBACK", "address the review")
    out = coder.load_context({"issue_text": "Fix the bug."})
    assert "Fix the bug." in out["task_text"]
    assert "address the review" in out["task_text"]


def test_load_context_folds_both_prior_summary_and_review_feedback():
    out = coder.load_context(
        {
            "issue_text": "Fix the bug.",
            "prior_summary": "Already found the root cause.",
            "review_feedback": "Remove the config section.",
        }
    )
    task_text = out["task_text"]
    assert "Fix the bug." in task_text
    assert "Already found the root cause." in task_text
    assert "Remove the config section." in task_text
    # Review feedback is the most recent, most actionable instruction: it comes
    # after the coder's own prior-turn progress.
    assert task_text.index("Already found the root cause.") < task_text.index("Remove the config section.")


# ---------------------------------------------------------------------------
# _docs_max_tokens (pure) -- per-round ceiling, not cumulative-spend-adjusted
# ---------------------------------------------------------------------------


def test_docs_max_tokens_passes_the_full_ceiling_regardless_of_coding_turn_spend():
    # Task 1 changed drive_turn's ceiling semantics to per-round (the largest
    # single round's input tokens), not a cumulative turn sum. The docs turn
    # is a SEPARATE DAC turn with its own per-round guard, so it must get the
    # same full ceiling the coding turn got -- not `ceiling - cumulative_spent`,
    # which could go to (near) zero after a big coding turn and trip the docs
    # turn's ceiling on its very first round, regardless of how large
    # max_tokens actually is.
    state = {"max_tokens": 100_000, "tokens_in": 95_000, "tokens_out": 4_999}

    assert coder._docs_max_tokens(state) == 100_000


def test_docs_max_tokens_is_none_when_no_ceiling_configured():
    assert coder._docs_max_tokens({"tokens_in": 10, "tokens_out": 5}) is None


def test_docs_max_tokens_ignores_absent_token_fields():
    assert coder._docs_max_tokens({"max_tokens": 50_000}) == 50_000


def test_docs_turn_receives_full_ceiling_after_large_coding_spend(tmp_path):
    # End-to-end: even though the coding turn spent almost the entire
    # ceiling, the docs turn's DAC call still gets the FULL max_tokens, not
    # the remainder.
    code_result = DacTurnResult(
        outcome_hint="completed", final_text="did the code", tokens_in=90_000, tokens_out=9_000
    )
    docs_result = DacTurnResult(outcome_hint="completed", final_text="did the docs", tokens_in=10, tokens_out=10)
    turn_calls: list[dict[str, Any]] = []
    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver_by_turn(code_result, docs_result, turn_calls),
        run_command=_base_runner(),
    )
    input_state = {**_needs_review_input(tmp_path), "max_tokens": 100_000}

    _order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-docs"}}))

    _assert_valid_result(result, blocked=False)
    docs_call = next(c for c in turn_calls if _is_docs_turn(c["config"]))
    assert docs_call["max_tokens"] == 100_000


# ---------------------------------------------------------------------------
# route_after_dac (pure)
# ---------------------------------------------------------------------------


def test_route_after_dac_proceeds_to_docs_when_completed_cleanly():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": False}
    assert coder.route_after_dac(state) == "run_docs"


def test_route_after_dac_routes_to_emit_result_on_interrupt():
    state = {"interrupt_payload": [{"x": 1}], "token_ceiling_exceeded": False}
    assert coder.route_after_dac(state) == "emit_result"


def test_route_after_dac_routes_to_emit_result_on_token_ceiling():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": True}
    assert coder.route_after_dac(state) == "emit_result"


# ---------------------------------------------------------------------------
# emit_result (pure)
# ---------------------------------------------------------------------------


def test_emit_result_needs_review_shape():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 0,
        "tokens_in": 10,
        "tokens_out": 20,
        "pr_url": "https://github.com/a/b/pull/1",
        "artifacts": ["src/x.py"],
        "dac_summary": "did the work",
        "committed": True,
    }
    out = coder.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.com/a/b/pull/1"
    assert result.artifacts == ["src/x.py"]
    assert result.turn_count == 1
    assert out["prior_summary"] == "did the work"


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
    out = coder.emit_result(state)
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
    out = coder.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability


def test_emit_result_requires_issue_run_and_thread_ids():
    with pytest.raises(KeyError):
        coder.emit_result({})


# ---------------------------------------------------------------------------
# commit (needs an injected run_command)
# ---------------------------------------------------------------------------


def test_commit_skips_git_commit_when_nothing_changed(tmp_path):
    runner = FakeRunner(
        rules=[(_starts_with("git", "status", "--porcelain"), coder.CommandResult(0, "", ""))]
    )
    node = coder.make_commit(runner)

    out = node({"cwd": str(tmp_path), "issue_id": "SPAC-1", "turn_count": 0})

    assert out["artifacts"] == []
    assert out["committed"] is False
    commit_calls = [c for c in runner.calls if c.argv[:2] == ["git", "commit"]]
    add_calls = [c for c in runner.calls if c.argv[:2] == ["git", "add"]]
    assert commit_calls == []
    assert len(add_calls) == 1


def test_commit_extracts_artifact_paths_and_commits_when_changed(tmp_path):
    runner = FakeRunner(
        rules=[
            (
                _starts_with("git", "status", "--porcelain"),
                coder.CommandResult(0, " M src/a.py\n?? src/b.py\n", ""),
            )
        ]
    )
    node = coder.make_commit(runner)

    out = node({"cwd": str(tmp_path), "issue_id": "SPAC-1", "turn_count": 0, "dac_summary": "added b.py"})

    assert set(out["artifacts"]) == {"src/a.py", "src/b.py"}
    assert out["committed"] is True
    commit_calls = [c for c in runner.calls if c.argv[:2] == ["git", "commit"]]
    assert len(commit_calls) == 1
    # No merge in progress -- the normal single-parent message form, never
    # the merge-completing `--no-edit`.
    assert commit_calls[0].argv[:3] == ["git", "commit", "-m"]


# ---------------------------------------------------------------------------
# commit completing an in-progress merge (Task 3): `merge_conflict_files`
# non-empty signals `sync_base` left a real merge in progress this turn
# (freshly conflicted, or resumed via its MERGE_HEAD guard) -- the resolved
# files are staged and the merge is concluded with `git commit --no-edit`
# instead of the normal single-parent commit.
# ---------------------------------------------------------------------------


def test_commit_completes_merge_with_no_edit_when_merge_conflict_files_set(tmp_path):
    runner = FakeRunner(
        rules=[
            (
                _starts_with("git", "status", "--porcelain"),
                coder.CommandResult(0, "M  src/a.py\nM  src/b.py\n", ""),
            )
        ]
    )
    node = coder.make_commit(runner)

    out = node(
        {
            "cwd": str(tmp_path),
            "issue_id": "SPAC-1",
            "turn_count": 0,
            "merge_conflict_files": ["src/a.py", "src/b.py"],
        }
    )

    assert set(out["artifacts"]) == {"src/a.py", "src/b.py"}
    assert out["committed"] is True

    add_calls = [c for c in runner.calls if c.argv[:2] == ["git", "add"]]
    assert len(add_calls) == 1
    assert add_calls[0].argv == ["git", "add", "-A"]
    # `git add` must be scoped to the coder's OWN worktree -- never the
    # clipse repo this test process itself runs from.
    assert add_calls[0].cwd == str(tmp_path)

    commit_calls = [c for c in runner.calls if c.argv[:2] == ["git", "commit"]]
    assert len(commit_calls) == 1
    assert commit_calls[0].argv == ["git", "commit", "--no-edit"]
    assert commit_calls[0].cwd == str(tmp_path)


def test_commit_completes_merge_even_when_status_looks_clean(tmp_path):
    # Edge case: a prior (interrupted) turn already staged the resolution,
    # so `git status --porcelain` reports nothing new this turn -- but the
    # merge is still unconcluded and must still be committed.
    runner = FakeRunner(rules=[(_starts_with("git", "status", "--porcelain"), coder.CommandResult(0, "", ""))])
    node = coder.make_commit(runner)

    out = node(
        {
            "cwd": str(tmp_path),
            "issue_id": "SPAC-1",
            "turn_count": 0,
            "merge_conflict_files": ["src/a.py"],
        }
    )

    assert out["committed"] is True
    commit_calls = [c for c in runner.calls if c.argv[:2] == ["git", "commit"]]
    assert len(commit_calls) == 1
    assert commit_calls[0].argv == ["git", "commit", "--no-edit"]
