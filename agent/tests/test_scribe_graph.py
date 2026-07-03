"""Tests for the Scribe lane's LangGraph graph (`clipse_agent.graphs.scribe`).

DAC (`dac.build_coder_agent` / `dac.drive_turn`) and git/gh are always faked
here via the graph's own dependency-injection seams (`agent_factory`,
`turn_driver`, `run_command`) -- these tests never touch a real model, a
real DAC agent, or a real subprocess/network call. `run_DAC` is the graph's
only async node, so the compiled graph must be driven with `.ainvoke`/
`.astream`; per `test_dac.py`'s convention, plain `asyncio.run` drives it
(no pytest-asyncio in this repo's approved dev deps). Fixtures mirror
`test_coder_graph.py`'s / `test_reviewer_graph.py`'s, adapted for the Scribe
lane's own gh call shapes and its wrote-docs-vs-no-op branch.
"""

from __future__ import annotations

import asyncio
import json
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import Any

import pytest
from langchain_core.messages import AIMessage
from langgraph.checkpoint.memory import InMemorySaver

from clipse_agent import dac
from clipse_agent.contract import BlockKind, Lane, Outcome, WorkerResult
from clipse_agent.dac import DacTurnResult
from clipse_agent.graphs import coder, reviewer, scribe

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
    changed_files: str = " M docs/architecture.md\n",
    pr_exists: bool = False,
    pr_url: str = "https://github.com/acme/widgets/pull/1",
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
# Happy path: wrote docs -> full node order, done + PR
# ---------------------------------------------------------------------------


def test_wrote_docs_runs_full_node_order_and_emits_done_with_pr(tmp_path):
    runner = _base_runner()
    agent_calls: list[dict[str, Any]] = []
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(
        outcome_hint="completed",
        final_text="Documented the new widget factory API.",
        tokens_in=80,
        tokens_out=200,
    )

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory(agent_calls),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )

    workspace = _worktree(tmp_path)
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "workspace": workspace,
        "issue_text": "Build the widget factory.",
    }
    config = {"configurable": {"thread_id": "thread-1"}}

    order, result = asyncio.run(_drive(graph, input_state, config))

    assert order == ["load_context", "ensure_worktree", "run_DAC", "commit", "push", "open_PR", "emit_result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.lane == Lane.scribe
    assert result.run_id == "run-1"
    assert result.issue_id == "SPAC-1"
    assert result.thread_id == "thread-1"
    assert result.turn_count == 1
    assert result.tokens.in_ == 80
    assert result.tokens.out == 200
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"
    assert result.artifacts == ["docs/architecture.md"]

    assert len(agent_calls) == 1
    assert agent_calls[0]["checkpointer"] is None
    assert agent_calls[0]["cwd"] == str(Path(workspace).resolve())
    assert agent_calls[0]["profile"].assistant_id == "clipse-scribe"


# ---------------------------------------------------------------------------
# No-op: DAC decides nothing needs documenting -> skip push/open_PR entirely
# ---------------------------------------------------------------------------


def test_noop_skips_push_and_open_pr_and_still_emits_done(tmp_path):
    runner = _base_runner(changed_files="")
    turn_result = DacTurnResult(
        outcome_hint="completed",
        final_text="Nothing user-facing changed; the docs are already accurate.",
        tokens_in=10,
        tokens_out=15,
    )

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-2",
        "run_id": "run-1",
        "thread_id": "thread-2",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-2"}}))

    assert order == ["load_context", "ensure_worktree", "run_DAC", "commit", "emit_result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.pr_url is None
    assert result.artifacts == []

    # No git commit and no gh call at all -- a no-op turn must never open an
    # empty PR just because it ran.
    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "commit") not in verbs
    assert ("git", "push") not in verbs
    gh_calls = [c for c in runner.calls if c.argv[0] == "gh"]
    assert gh_calls == []


# ---------------------------------------------------------------------------
# open_PR: idempotent reuse, and never opened as a draft (no lane ever
# reviews/un-drafts a documentation PR)
# ---------------------------------------------------------------------------


def test_open_pr_reuses_existing_pr_when_gh_pr_view_succeeds(tmp_path):
    runner = _base_runner(pr_exists=True, pr_url="https://github.com/acme/widgets/pull/7")
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-3",
        "run_id": "run-1",
        "thread_id": "thread-3",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    _, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-3"}}))

    assert result.pr_url == "https://github.com/acme/widgets/pull/7"
    view_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "view"]]
    create_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "create"]]
    assert len(view_calls) == 1
    assert create_calls == []


def test_open_pr_creates_without_draft_flag_when_gh_pr_view_finds_nothing(tmp_path):
    runner = _base_runner(pr_exists=False, pr_url="https://github.com/acme/widgets/pull/9")
    turn_result = DacTurnResult(outcome_hint="completed", final_text="Documented X.", tokens_in=1, tokens_out=1)

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-4",
        "run_id": "run-1",
        "thread_id": "thread-4",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    _, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-4"}}))

    assert result.pr_url == "https://github.com/acme/widgets/pull/9"
    create_calls = [c for c in runner.calls if c.argv[:3] == ["gh", "pr", "create"]]
    assert len(create_calls) == 1
    assert "--head" in create_calls[0].argv
    assert "clipse/spac-1" in create_calls[0].argv
    assert "--base" in create_calls[0].argv
    assert "SPAC-4" in create_calls[0].argv[create_calls[0].argv.index("--title") + 1]
    # Unlike Coder's draft PRs (a Reviewer lane un-drafts them): no lane ever
    # reviews a documentation PR, so this must never open as a draft.
    assert "--draft" not in create_calls[0].argv


# ---------------------------------------------------------------------------
# Interrupt -> blocked(needs_input); skips commit/push/gh entirely
# ---------------------------------------------------------------------------


def test_interrupt_emits_blocked_needs_input_and_skips_commit_and_gh(tmp_path):
    runner = _base_runner()
    turn_result = DacTurnResult(
        outcome_hint="interrupted",
        final_text="paused, needs a decision",
        tokens_in=10,
        tokens_out=5,
        interrupt_payload=[{"action_requests": [{"name": "shell", "args": {"command": "rm -rf /"}}]}],
    )

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-5",
        "run_id": "run-1",
        "thread_id": "thread-5",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-5"}}))

    assert order == ["load_context", "ensure_worktree", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.needs_input
    assert result.pr_url is None
    assert result.tokens.in_ == 10
    assert result.tokens.out == 5

    verbs = [tuple(c.argv[:2]) for c in runner.calls]
    assert ("git", "commit") not in verbs
    assert ("git", "push") not in verbs
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
    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-6",
        "run_id": "run-1",
        "thread_id": "thread-6",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "max_tokens": 1000,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-6"}}))

    assert order == ["load_context", "ensure_worktree", "run_DAC", "emit_result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability
    assert "1100" in result.summary


# ---------------------------------------------------------------------------
# Resume (continuation after a prior interrupt)
# ---------------------------------------------------------------------------


def test_resume_turn_drives_dac_with_resume_payload_not_task_text(tmp_path):
    runner = _base_runner()
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="resumed and finished.", tokens_in=5, tokens_out=5)

    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=runner,
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-7",
        "run_id": "run-2",
        "thread_id": "thread-7",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
        "resume_payload": {"int-1": {"decisions": [{"type": "approve"}]}},
        "turn_count": 1,
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-7"}}))

    assert order[:3] == ["load_context", "ensure_worktree", "run_DAC"]
    assert turn_calls[0]["resume"] == {"int-1": {"decisions": [{"type": "approve"}]}}
    assert "task_text" not in turn_calls[0]
    assert result.turn_count == 2


# ---------------------------------------------------------------------------
# DAC agent build: default wiring reaches the real clipse_agent.dac module,
# reusing the same safety-critical create_cli_agent invariants as the other
# lanes, with the Scribe's own profile.
# ---------------------------------------------------------------------------


def test_build_scribe_graph_default_wiring_uses_real_dac_module_with_safety_invariants(tmp_path, monkeypatch):
    class _FakeAgentGraph:
        async def astream(self, stream_input: Any, **kwargs: Any):
            yield (
                (),
                "messages",
                (
                    AIMessage(
                        content=[{"type": "text", "text": "No docs changes needed."}],
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

    runner = _base_runner(changed_files="")
    graph = scribe.build_scribe_graph(run_command=runner)

    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-10",
        "run_id": "run-1",
        "thread_id": "thread-10",
        "workspace": _worktree(tmp_path),
        "issue_text": "Build the thing.",
    }

    order, result = asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "thread-10"}}))

    assert order == ["load_context", "ensure_worktree", "run_DAC", "commit", "emit_result"]
    assert result.outcome == Outcome.done
    assert result.tokens.in_ == 7
    assert result.tokens.out == 3

    assert captured["assistant_id"] == "clipse-scribe"
    # Safety invariant enforced inside dac.build_coder_agent must never be
    # bypassed just because scribe.py is doing the calling.
    assert captured["kwargs"]["auto_approve"] is False
    assert captured["kwargs"]["interrupt_shell_only"] is True
    assert captured["kwargs"]["enable_ask_user"] is True
    assert set(captured["kwargs"]["shell_allow_list"]) == {"git", "gh", "ls", "cat", "grep", "rg", "find", "mkdir"}
    assert "python" not in captured["kwargs"]["shell_allow_list"]


def test_run_dac_forwards_profile_and_checkpointer(tmp_path):
    agent_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.", tokens_in=1, tokens_out=1)
    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory(agent_calls),
        turn_driver=_fake_turn_driver(turn_result, []),
        run_command=_base_runner(),
    )
    workspace = _worktree(tmp_path)
    input_state: scribe.ScribeState = {
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
    assert agent_calls[0]["profile"].assistant_id == "clipse-scribe"


# ---------------------------------------------------------------------------
# Cross-lane checkpoint-thread safety: the Scribe's own inner DAC thread must
# never collide with the Coder's or the Reviewer's, even when all three
# lanes' runs share one physical checkpoint DB and the same outer thread_id
# for a given issue (see graphs/coder.py's _DAC_THREAD_NAMESPACE_SUFFIX and
# graphs/reviewer.py's _REVIEW_DAC_THREAD_NAMESPACE_SUFFIX docstrings for
# exactly which cross-lane collision this prevents).
# ---------------------------------------------------------------------------


def test_run_dac_uses_a_scribe_specific_dac_thread_namespace_distinct_from_coder_and_reviewer(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="ok.", tokens_in=1, tokens_out=1)
    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(turn_result, turn_calls),
        run_command=_base_runner(),
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-12",
        "run_id": "run-1",
        "thread_id": "shared-thread",
        "workspace": _worktree(tmp_path),
        "issue_text": "x",
    }

    asyncio.run(_drive(graph, input_state, {"configurable": {"thread_id": "shared-thread"}}))

    dac_thread_id = turn_calls[0]["config"]["configurable"]["thread_id"]
    assert dac_thread_id == "shared-thread::scribe-dac"
    # Compare against the other two lanes' *actual* suffixes (not hardcoded
    # copies), so this guard can't silently go stale if either ever changes.
    coder_dac_thread_id = f"shared-thread{coder._DAC_THREAD_NAMESPACE_SUFFIX}"
    reviewer_dac_thread_id = f"shared-thread{reviewer._REVIEW_DAC_THREAD_NAMESPACE_SUFFIX}"
    assert dac_thread_id != coder_dac_thread_id
    assert dac_thread_id != reviewer_dac_thread_id
    assert coder_dac_thread_id != reviewer_dac_thread_id


# ---------------------------------------------------------------------------
# ensure_worktree validation (reused from graphs.coder; smoke-tested here to
# prove it is actually wired into this graph)
# ---------------------------------------------------------------------------


def test_ensure_worktree_raises_when_workspace_missing(tmp_path):
    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver(
            DacTurnResult(outcome_hint="completed", final_text="", tokens_in=0, tokens_out=0), []
        ),
        run_command=_base_runner(),
    )
    input_state: scribe.ScribeState = {
        "issue_id": "SPAC-13",
        "run_id": "run-1",
        "thread_id": "thread-13",
        "workspace": str(tmp_path / "does-not-exist"),
        "issue_text": "x",
    }
    with pytest.raises(scribe.ScribeGraphError, match="does not exist"):
        asyncio.run(graph.ainvoke(input_state, {"configurable": {"thread_id": "thread-13"}}))


# ---------------------------------------------------------------------------
# route_after_dac / route_after_commit (pure)
# ---------------------------------------------------------------------------


def test_route_after_dac_proceeds_to_commit_when_completed_cleanly():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": False}
    assert scribe.route_after_dac(state) == "commit"


def test_route_after_dac_routes_to_emit_result_on_interrupt():
    state = {"interrupt_payload": [{"x": 1}], "token_ceiling_exceeded": False}
    assert scribe.route_after_dac(state) == "emit_result"


def test_route_after_dac_routes_to_emit_result_on_token_ceiling():
    state = {"interrupt_payload": None, "token_ceiling_exceeded": True}
    assert scribe.route_after_dac(state) == "emit_result"


def test_route_after_commit_proceeds_to_push_when_committed():
    assert scribe.route_after_commit({"committed": True}) == "push"


def test_route_after_commit_skips_to_emit_result_when_noop():
    assert scribe.route_after_commit({"committed": False}) == "emit_result"


# ---------------------------------------------------------------------------
# emit_result (pure)
# ---------------------------------------------------------------------------


def test_emit_result_done_shape_when_docs_written():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 0,
        "tokens_in": 10,
        "tokens_out": 20,
        "committed": True,
        "artifacts": ["docs/x.md"],
        "dac_summary": "wrote docs",
        "pr_url": "https://github.com/acme/widgets/pull/1",
    }
    out = scribe.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.lane == Lane.scribe
    assert result.turn_count == 1
    assert result.pr_url == "https://github.com/acme/widgets/pull/1"
    assert result.artifacts == ["docs/x.md"]
    assert out["prior_summary"] == "wrote docs"


def test_emit_result_done_shape_when_noop():
    state = {
        "issue_id": "SPAC-1",
        "run_id": "run-1",
        "thread_id": "thread-1",
        "turn_count": 0,
        "tokens_in": 10,
        "tokens_out": 20,
        "committed": False,
        "artifacts": [],
        "dac_summary": "",
    }
    out = scribe.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=False)
    assert result.outcome == Outcome.done
    assert result.pr_url is None
    assert result.artifacts == []
    assert result.summary  # never empty, even with no dac_summary text


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
    out = scribe.emit_result(state)
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
    out = scribe.emit_result(state)
    result = out["result"]
    _assert_valid_result(result, blocked=True)
    assert result.block_kind == BlockKind.capability


def test_emit_result_requires_issue_run_and_thread_ids():
    with pytest.raises(KeyError):
        scribe.emit_result({})


# ---------------------------------------------------------------------------
# Checkpointer-driven state carryover (compiled with a checkpointer)
# ---------------------------------------------------------------------------


def test_checkpointer_scopes_state_by_thread_id(tmp_path):
    async def turn_driver(agent_graph: Any, config: Any, **kwargs: Any) -> DacTurnResult:
        return DacTurnResult(outcome_hint="completed", final_text="turn output.", tokens_in=1, tokens_out=1)

    checkpointer = InMemorySaver()
    graph = scribe.build_scribe_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=turn_driver,
        run_command=_base_runner(),
        checkpointer=checkpointer,
    )
    workspace = _worktree(tmp_path)
    base_input: scribe.ScribeState = {
        "issue_id": "SPAC-14",
        "run_id": "run-1",
        "thread_id": "thread-14",
        "workspace": workspace,
        "issue_text": "Document X.",
    }

    async def drive() -> tuple[WorkerResult, WorkerResult]:
        first = await graph.ainvoke(base_input, {"configurable": {"thread_id": "thread-14"}})
        other_issue = {**base_input, "issue_id": "SPAC-15", "thread_id": "thread-15"}
        second = await graph.ainvoke(other_issue, {"configurable": {"thread_id": "thread-15"}})
        return first["result"], second["result"]

    first_result, second_result = asyncio.run(drive())
    assert first_result.turn_count == 1
    assert second_result.turn_count == 1  # a fresh thread never sees issue-14's history
