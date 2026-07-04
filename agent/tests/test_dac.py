"""Tests for the DAC engine wrapper (`clipse_agent.dac`).

`create_cli_agent` and the agent graph's `.astream` are always faked here —
these tests exercise the wrapper's safety-critical wiring and its
stream-driving logic, never a real model, real DAC agent, or real network
call. `drive_turn` is async; plain `asyncio.run` drives it since the repo's
approved Python dev deps are `pytest` + `ruff` only (no `pytest-asyncio`).
"""

from __future__ import annotations

import asyncio
import dataclasses
from types import SimpleNamespace
from typing import Any

import pytest
from langchain_core.messages import AIMessage, ToolMessage
from langgraph.types import Command, Interrupt

from clipse_agent import dac
from clipse_agent.profiles.coder import get_coder_profile

_CONFIG: dict[str, Any] = {"configurable": {"thread_id": "thread-1"}}


def _ai_message(text: str, *, tokens_in: int, tokens_out: int) -> AIMessage:
    return AIMessage(
        content=[{"type": "text", "text": text}],
        usage_metadata={
            "input_tokens": tokens_in,
            "output_tokens": tokens_out,
            "total_tokens": tokens_in + tokens_out,
        },
    )


def _interrupt(value: Any, interrupt_id: str = "int-1") -> Interrupt:
    return Interrupt(value=value, id=interrupt_id)


class _FakeAgentGraph:
    """Stand-in for the `Pregel` graph `create_cli_agent` returns.

    Records every `astream` call's input/kwargs and replays a canned list
    of `(namespace, mode, data)` chunks. Any `BaseException` instance in
    `chunks` is raised instead of yielded, to simulate a mid-stream
    failure. `consumed` records only the chunks actually pulled, so a test
    can prove the stream was abandoned early.
    """

    def __init__(self, chunks: list[Any]) -> None:
        self._chunks = chunks
        self.calls: list[dict[str, Any]] = []
        self.consumed: list[Any] = []

    async def astream(self, stream_input: Any, **kwargs: Any):
        self.calls.append({"stream_input": stream_input, **kwargs})
        for chunk in self._chunks:
            if isinstance(chunk, BaseException):
                raise chunk
            self.consumed.append(chunk)
            yield chunk


# ---------------------------------------------------------------------------
# build_coder_agent
# ---------------------------------------------------------------------------


def test_build_coder_agent_uses_safe_shell_enforcement(monkeypatch):
    captured: dict[str, Any] = {}

    def fake_create_cli_agent(model, assistant_id, **kwargs):
        captured["model"] = model
        captured["assistant_id"] = assistant_id
        captured["kwargs"] = kwargs
        return ("agent-graph", "backend")

    monkeypatch.setattr(dac, "create_cli_agent", fake_create_cli_agent)

    profile = get_coder_profile()
    result = dac.build_coder_agent(profile, checkpointer=None, cwd="/tmp/work")

    assert result == ("agent-graph", "backend")
    assert captured["model"] == profile.model
    assert captured["assistant_id"] == profile.assistant_id

    kwargs = captured["kwargs"]
    # SAFETY (non-negotiable): auto_approve=True silently drops the shell
    # allow-list (deepagents_code.agent:1336/1597/1612) -- this combination
    # must never appear. See the design doc's "DAC API spike findings".
    assert kwargs["auto_approve"] is False
    assert kwargs["interrupt_shell_only"] is True
    # enable_ask_user MUST be True: AskUserMiddleware (ask_user.py:256) is the
    # only interrupt source once the shell middleware forces interrupt_on={},
    # so it is the sole way the agent surfaces a __interrupt__ that the worker
    # maps to blocked(needs_input). False makes that path dead code.
    assert kwargs["enable_ask_user"] is True
    assert kwargs["enable_shell"] is True
    assert kwargs["shell_allow_list"] == list(profile.shell_allow_list)
    assert kwargs["interactive"] is False
    assert kwargs["system_prompt"] == profile.system_prompt
    assert kwargs["cwd"] == "/tmp/work"
    assert kwargs["checkpointer"] is None


def test_build_coder_agent_forwards_kernel_owned_checkpointer(monkeypatch):
    captured: dict[str, Any] = {}
    sentinel_checkpointer = object()

    def fake_create_cli_agent(model, assistant_id, **kwargs):
        captured["kwargs"] = kwargs
        return ("agent-graph", "backend")

    monkeypatch.setattr(dac, "create_cli_agent", fake_create_cli_agent)

    profile = get_coder_profile()
    dac.build_coder_agent(profile, checkpointer=sentinel_checkpointer, cwd="/work/issue-1")

    assert captured["kwargs"]["checkpointer"] is sentinel_checkpointer
    assert captured["kwargs"]["cwd"] == "/work/issue-1"


def test_build_coder_agent_wraps_create_cli_agent_errors(monkeypatch):
    def boom(*args, **kwargs):
        raise ValueError("bad model spec")

    monkeypatch.setattr(dac, "create_cli_agent", boom)

    profile = get_coder_profile()
    with pytest.raises(dac.DacError, match="bad model spec") as exc_info:
        dac.build_coder_agent(profile, checkpointer=None, cwd="/tmp/work")

    assert isinstance(exc_info.value.__cause__, ValueError)


def test_build_coder_agent_codex_prebuilds_model_object(monkeypatch):
    # init_chat_model can't resolve the DAC-only "openai_codex" provider, so
    # build_coder_agent must hand create_cli_agent the pre-built BaseChatModel
    # object from create_model, never the raw "openai_codex:..." string.
    sentinel = object()
    monkeypatch.setattr(dac, "create_model", lambda spec: SimpleNamespace(model=sentinel))
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model) or (object(), object()),
    )

    dac.build_coder_agent(get_coder_profile(model="openai_codex:gpt-5.5"), None, "/tmp")

    assert captured["model"] is sentinel  # object, never the string


def test_build_coder_agent_anthropic_passes_string(monkeypatch):
    # Every other provider keeps the plain-string path untouched -- create_model
    # must not even be called.
    called = {"create_model": False}
    monkeypatch.setattr(
        dac, "create_model", lambda spec: called.__setitem__("create_model", True)
    )
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model) or (object(), object()),
    )

    dac.build_coder_agent(get_coder_profile(model="anthropic:claude-sonnet-4-6"), None, "/tmp")

    assert captured["model"] == "anthropic:claude-sonnet-4-6"
    assert called["create_model"] is False


def test_build_coder_agent_wraps_create_model_failure(monkeypatch):
    def boom(spec):
        raise RuntimeError("no creds")

    monkeypatch.setattr(dac, "create_model", boom)

    with pytest.raises(dac.DacError) as exc_info:
        dac.build_coder_agent(get_coder_profile(model="openai_codex:gpt-5.5"), None, "/tmp")

    assert isinstance(exc_info.value.__cause__, RuntimeError)


# ---------------------------------------------------------------------------
# drive_turn -- input validation
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "kwargs",
    [
        pytest.param({}, id="neither-task_text-nor-resume"),
        pytest.param(
            {"task_text": "fix it", "resume": {"int-1": {}}}, id="both-task_text-and-resume"
        ),
    ],
)
def test_drive_turn_requires_exactly_one_of_task_text_or_resume(kwargs):
    graph = _FakeAgentGraph([])

    with pytest.raises(ValueError, match="exactly one"):
        asyncio.run(dac.drive_turn(graph, _CONFIG, max_tokens=None, **kwargs))

    # Must fail before ever touching the graph.
    assert graph.calls == []


# ---------------------------------------------------------------------------
# drive_turn -- stream input construction
# ---------------------------------------------------------------------------


def test_drive_turn_sends_fresh_turn_input_from_task_text():
    graph = _FakeAgentGraph([])

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="fix the bug", max_tokens=None))

    assert len(graph.calls) == 1
    call = graph.calls[0]
    assert call["stream_input"] == {"messages": [{"role": "user", "content": "fix the bug"}]}
    assert call["stream_mode"] == ["messages", "updates"]
    assert call["subgraphs"] is True
    assert call["config"] == _CONFIG


def test_drive_turn_sends_resume_command_from_resume_payload():
    graph = _FakeAgentGraph([])
    payload = {"int-1": {"decisions": [{"type": "approve"}]}}

    asyncio.run(dac.drive_turn(graph, _CONFIG, resume=payload, max_tokens=None))

    assert graph.calls[0]["stream_input"] == Command(resume=payload)


# ---------------------------------------------------------------------------
# drive_turn -- interrupt detection
# ---------------------------------------------------------------------------


def test_drive_turn_completes_when_no_interrupt_is_seen():
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("all done", tokens_in=10, tokens_out=5), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.outcome_hint == "completed"
    assert result.interrupt_payload is None
    assert result.token_ceiling_exceeded is False
    assert result.final_text == "all done"
    assert result.tokens_in == 10
    assert result.tokens_out == 5


def test_drive_turn_detects_interrupt():
    action = {"action_requests": [{"name": "shell", "args": {"command": "rm -rf /"}}]}
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("checking first", tokens_in=3, tokens_out=1), {})),
            ((), "updates", {"__interrupt__": [_interrupt(action)]}),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.outcome_hint == "interrupted"
    assert result.interrupt_payload == [action]
    assert result.token_ceiling_exceeded is False


def test_drive_turn_ignores_empty_interrupt_list():
    # `__interrupt__` can be present with an empty list; deepagents_code's
    # own `_process_interrupts` only flags a real interrupt when the list
    # is non-empty (`if interrupts:`). A vacuous key must not trip us.
    graph = _FakeAgentGraph(
        [
            ((), "updates", {"__interrupt__": []}),
            ((), "messages", (_ai_message("done", tokens_in=1, tokens_out=1), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.outcome_hint == "completed"
    assert result.interrupt_payload is None


# ---------------------------------------------------------------------------
# drive_turn -- token + text accumulation
# ---------------------------------------------------------------------------


def test_drive_turn_aggregates_tokens_and_text_across_chunks():
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("hello ", tokens_in=10, tokens_out=2), {})),
            ((), "messages", (_ai_message("world", tokens_in=7, tokens_out=3), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.tokens_in == 17
    assert result.tokens_out == 5
    assert result.final_text == "hello world"


def test_drive_turn_ignores_non_ai_messages():
    # A ToolMessage has no usage_metadata, and its content_blocks are tool
    # output, not assistant text -- neither should be folded in.
    tool_msg = ToolMessage(content="tool output text", tool_call_id="call-1")
    graph = _FakeAgentGraph([((), "messages", (tool_msg, {}))])

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.tokens_in == 0
    assert result.tokens_out == 0
    assert result.final_text == ""


# ---------------------------------------------------------------------------
# drive_turn -- max_tokens ceiling
# ---------------------------------------------------------------------------


def test_drive_turn_trips_max_tokens_ceiling_and_stops_early():
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("a", tokens_in=40, tokens_out=10), {})),  # 50
            ((), "messages", (_ai_message("b", tokens_in=40, tokens_out=20), {})),  # 110 total
            ((), "messages", (_ai_message("c", tokens_in=1000, tokens_out=1000), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is True
    assert result.outcome_hint == "interrupted"
    assert result.tokens_in == 80
    assert result.tokens_out == 30
    # The third chunk must never be pulled once the ceiling trips.
    assert len(graph.consumed) == 2


def test_drive_turn_never_trips_ceiling_when_max_tokens_is_none():
    graph = _FakeAgentGraph(
        [((), "messages", (_ai_message("a", tokens_in=10_000, tokens_out=10_000), {}))]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.token_ceiling_exceeded is False
    assert result.outcome_hint == "completed"


def test_drive_turn_ceiling_is_exceeded_not_reached_boundary():
    # Landing exactly on max_tokens must not trip the ceiling ("exceed").
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("a", tokens_in=50, tokens_out=50), {})),
            ((), "messages", (_ai_message("b", tokens_in=1, tokens_out=0), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is True
    assert len(graph.consumed) == 2
    assert result.tokens_in == 51


# ---------------------------------------------------------------------------
# drive_turn -- error wrapping
# ---------------------------------------------------------------------------


def test_drive_turn_wraps_streaming_errors_with_context():
    graph = _FakeAgentGraph([RuntimeError("boom from langgraph")])

    with pytest.raises(dac.DacError, match="boom from langgraph") as exc_info:
        asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert isinstance(exc_info.value.__cause__, RuntimeError)
    assert "thread-1" in str(exc_info.value)


# ---------------------------------------------------------------------------
# DacTurnResult
# ---------------------------------------------------------------------------


def test_dac_turn_result_is_frozen():
    result = dac.DacTurnResult(outcome_hint="completed", final_text="", tokens_in=0, tokens_out=0)

    assert dataclasses.is_dataclass(result)
    with pytest.raises(dataclasses.FrozenInstanceError):
        result.tokens_in = 5


def test_dac_turn_result_defaults_interrupt_payload_and_ceiling_flag():
    result = dac.DacTurnResult(outcome_hint="completed", final_text="ok", tokens_in=1, tokens_out=1)

    assert result.interrupt_payload is None
    assert result.token_ceiling_exceeded is False
