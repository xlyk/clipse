"""Tests for the DAC engine wrapper (`clipse_agent.dac`).

`create_cli_agent` and the agent graph's `.astream` are always faked here —
these tests exercise the wrapper's safety-critical wiring and its
stream-driving logic, never a real model, real DAC agent, or real network
call. `drive_turn` is async; plain `asyncio.run` drives it since the repo's
approved Python dev deps are `pytest` + `ruff` only (no `pytest-asyncio`).

One exception:
`test_summarization_middleware_is_installed_with_our_lowered_trigger` builds
a REAL `create_cli_agent`/`create_deep_agent` graph (a regression guard that
DAC's built-in auto-summarizer is still there after a dependency bump) --
still zero network, since building the graph's structure never calls the
model, and only a placeholder API key string is needed to satisfy
`init_chat_model`'s construction-time credential check.
"""

from __future__ import annotations

import asyncio
import dataclasses
from types import SimpleNamespace
from typing import Any

import deepagents.graph as deepagents_graph
import pytest
from deepagents.middleware.summarization import (
    create_summarization_middleware as _real_create_summarization_middleware,
)
from langchain_core.messages import AIMessage, ToolMessage
from langgraph.types import Command, Interrupt

from clipse_agent import dac
from clipse_agent.profiles.coder import _SHELL_ALLOW_LIST, get_coder_profile

_CONFIG: dict[str, Any] = {"configurable": {"thread_id": "thread-1"}}


def _ai_message(
    text: str, *, tokens_in: int, tokens_out: int, message_id: str | None = None
) -> AIMessage:
    return AIMessage(
        id=message_id,
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


def test_build_coder_agent_unrestricted_profile_uses_auto_approve(monkeypatch):
    # New default mode (decision 2026-07-07): profile.shell_allow_list is
    # None (get_coder_profile's own default) -- build_coder_agent must route
    # to DAC's auto_approve=True, no allow-list, no interrupt_shell_only.
    captured: dict[str, Any] = {}

    def fake_create_cli_agent(model, assistant_id, **kwargs):
        captured["model"] = model
        captured["assistant_id"] = assistant_id
        captured["kwargs"] = kwargs
        return ("agent-graph", "backend")

    monkeypatch.setattr(dac, "create_cli_agent", fake_create_cli_agent)
    # context_window_tokens defaults on (200_000), so build_coder_agent always
    # resolves the model via create_model now -- fake it to a model-like
    # object (not a bare `object()`) so the profile-mutation branch has
    # somewhere real to write.
    model_stub = SimpleNamespace(profile=None)
    monkeypatch.setattr(dac, "create_model", lambda spec, **kw: SimpleNamespace(model=model_stub))

    profile = get_coder_profile()
    assert profile.shell_allow_list is None
    result = dac.build_coder_agent(profile, checkpointer=None, cwd="/tmp/work")

    assert result == ("agent-graph", "backend")
    assert captured["model"] is model_stub
    assert captured["assistant_id"] == profile.assistant_id

    kwargs = captured["kwargs"]
    assert kwargs["auto_approve"] is True
    assert kwargs["interrupt_shell_only"] is False
    assert kwargs.get("shell_allow_list") in (None, [])
    # enable_ask_user MUST be True in BOTH modes: AskUserMiddleware
    # (ask_user.py:256) is the sole source of a LangGraph interrupt() once
    # auto_approve forces interrupt_on={}, so it is the only way the agent
    # surfaces a __interrupt__ that the worker maps to blocked(needs_input).
    # False makes that path dead code.
    assert kwargs["enable_ask_user"] is True
    assert kwargs["enable_shell"] is True
    assert kwargs["interactive"] is False
    assert kwargs["system_prompt"] == profile.system_prompt
    assert kwargs["cwd"] == "/tmp/work"
    assert kwargs["checkpointer"] is None


def test_build_coder_agent_restrictive_profile_keeps_safety_combo(monkeypatch):
    # Restrictive mode (unchanged invariant): a profile carrying an explicit
    # shell_allow_list must still get the original safe combination.
    captured: dict[str, Any] = {}

    def fake_create_cli_agent(model, assistant_id, **kwargs):
        captured["model"] = model
        captured["assistant_id"] = assistant_id
        captured["kwargs"] = kwargs
        return ("agent-graph", "backend")

    monkeypatch.setattr(dac, "create_cli_agent", fake_create_cli_agent)
    # context_window_tokens defaults on (200_000), so build_coder_agent always
    # resolves the model via create_model now -- fake it to a model-like
    # object (not a bare `object()`) so the profile-mutation branch has
    # somewhere real to write.
    model_stub = SimpleNamespace(profile=None)
    monkeypatch.setattr(dac, "create_model", lambda spec, **kw: SimpleNamespace(model=model_stub))

    profile = get_coder_profile(shell_allow_list=("git", "gh"))
    result = dac.build_coder_agent(profile, checkpointer=None, cwd="/tmp/work")

    assert result == ("agent-graph", "backend")
    assert captured["model"] is model_stub
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
    assert kwargs["shell_allow_list"] == ["git", "gh"]
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
    monkeypatch.setattr(dac, "create_model", lambda spec, **kw: SimpleNamespace(model=SimpleNamespace(profile=None)))

    profile = get_coder_profile()
    dac.build_coder_agent(profile, checkpointer=sentinel_checkpointer, cwd="/work/issue-1")

    assert captured["kwargs"]["checkpointer"] is sentinel_checkpointer
    assert captured["kwargs"]["cwd"] == "/work/issue-1"


def test_build_coder_agent_wraps_create_cli_agent_errors(monkeypatch):
    def boom(*args, **kwargs):
        raise ValueError("bad model spec")

    monkeypatch.setattr(dac, "create_cli_agent", boom)
    monkeypatch.setattr(dac, "create_model", lambda spec, **kw: SimpleNamespace(model=SimpleNamespace(profile=None)))

    profile = get_coder_profile()
    with pytest.raises(dac.DacError, match="bad model spec") as exc_info:
        dac.build_coder_agent(profile, checkpointer=None, cwd="/tmp/work")

    assert isinstance(exc_info.value.__cause__, ValueError)


def test_build_coder_agent_codex_prebuilds_model_object(monkeypatch):
    # init_chat_model can't resolve the DAC-only "openai_codex" provider, so
    # build_coder_agent must hand create_cli_agent the pre-built BaseChatModel
    # object from create_model, never the raw "openai_codex:..." string.
    sentinel = SimpleNamespace()  # model-like: needs a settable `.profile` --
    # context_window_tokens defaults on, so build_coder_agent always writes to it.
    captured_create_model: dict[str, Any] = {}

    def fake_create_model(spec, *, extra_kwargs=None, **kwargs):
        captured_create_model["spec"] = spec
        captured_create_model["extra_kwargs"] = extra_kwargs
        return SimpleNamespace(model=sentinel)

    monkeypatch.setattr(dac, "create_model", fake_create_model)
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model) or (object(), object()),
    )

    dac.build_coder_agent(get_coder_profile(model="openai_codex:gpt-5.5"), None, "/tmp")

    assert captured["model"] is sentinel  # object, never the string
    # No model_params on the profile -- extra_kwargs must stay falsy, exactly
    # the pre-model_params behavior.
    assert not captured_create_model["extra_kwargs"]


def test_build_coder_agent_codex_with_model_params_passes_extra_kwargs(monkeypatch):
    # A codex profile carrying model_params must forward them verbatim as
    # create_model's extra_kwargs -- this is how per-lane reasoning_effort
    # etc. reach the constructed model.
    model_params = {"reasoning_effort": "low"}
    captured_create_model: dict[str, Any] = {}

    def fake_create_model(spec, *, extra_kwargs=None, **kwargs):
        captured_create_model["spec"] = spec
        captured_create_model["extra_kwargs"] = extra_kwargs
        return SimpleNamespace(model=SimpleNamespace())

    monkeypatch.setattr(dac, "create_model", fake_create_model)
    monkeypatch.setattr(dac, "create_cli_agent", lambda model, aid, **kw: (object(), object()))

    profile = get_coder_profile(model="openai_codex:gpt-5.5", model_params=model_params)
    dac.build_coder_agent(profile, None, "/tmp")

    assert captured_create_model["spec"] == "openai_codex:gpt-5.5"
    assert captured_create_model["extra_kwargs"] == model_params


def test_build_coder_agent_default_profile_routes_through_create_model(monkeypatch):
    # context_window_tokens defaults to 200_000 (not None), so even a plain
    # Anthropic profile with no model_params now routes through create_model
    # by default -- create_cli_agent needs a model *object* (not a bare spec
    # string) to carry the mutated `.profile`. This supersedes the old
    # "every other provider keeps the plain-string path" assumption; see
    # test_build_coder_agent_opts_out_of_create_model_when_context_window_tokens_is_none
    # for the (now opt-in) plain-string path.
    captured_create_model: dict[str, Any] = {}
    model_stub = SimpleNamespace(profile=None)

    def fake_create_model(spec, *, extra_kwargs=None, **kwargs):
        captured_create_model["spec"] = spec
        captured_create_model["extra_kwargs"] = extra_kwargs
        return SimpleNamespace(model=model_stub)

    monkeypatch.setattr(dac, "create_model", fake_create_model)
    monkeypatch.setattr(dac, "create_cli_agent", lambda model, aid, **kw: (object(), object()))

    dac.build_coder_agent(get_coder_profile(model="anthropic:claude-sonnet-4-6"), None, "/tmp")

    assert captured_create_model["spec"] == "anthropic:claude-sonnet-4-6"
    assert not captured_create_model["extra_kwargs"]
    assert model_stub.profile == {"max_input_tokens": 200_000}


def test_build_coder_agent_opts_out_of_create_model_when_context_window_tokens_is_none(monkeypatch):
    # A hand-built profile can still opt all the way out of the trigger-
    # lowering lever: with context_window_tokens=None and no model_params,
    # the plain-string path is preserved -- create_model must not even be
    # called. (get_coder_profile(context_window_tokens=None) still yields the
    # default, exactly like `model`'s own idiom -- only a direct
    # dataclasses.replace/CoderProfile construction can produce this.)
    called = {"create_model": False}
    monkeypatch.setattr(
        dac, "create_model", lambda spec, **kw: called.__setitem__("create_model", True)
    )
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model) or (object(), object()),
    )

    profile = dataclasses.replace(
        get_coder_profile(model="anthropic:claude-sonnet-4-6"), context_window_tokens=None
    )
    dac.build_coder_agent(profile, None, "/tmp")

    assert captured["model"] == "anthropic:claude-sonnet-4-6"
    assert called["create_model"] is False


def test_build_coder_agent_anthropic_with_model_params_routes_through_create_model(monkeypatch):
    # ANY lane with model_params must route through create_model, not just
    # codex -- extra_kwargs is create_model's documented param and the only
    # way per-lane thinking/reasoning params reach the constructed model
    # object, so a non-codex provider with model_params must also give up
    # the plain-string path.
    sentinel = SimpleNamespace()  # settable `.profile`, see the codex test above
    model_params = {"thinking": {"type": "enabled", "budget_tokens": 2048}}
    captured_create_model: dict[str, Any] = {}

    def fake_create_model(spec, *, extra_kwargs=None, **kwargs):
        captured_create_model["spec"] = spec
        captured_create_model["extra_kwargs"] = extra_kwargs
        return SimpleNamespace(model=sentinel)

    monkeypatch.setattr(dac, "create_model", fake_create_model)
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model) or (object(), object()),
    )

    profile = get_coder_profile(model="anthropic:claude-sonnet-4-6", model_params=model_params)
    dac.build_coder_agent(profile, None, "/tmp")

    assert captured_create_model["spec"] == "anthropic:claude-sonnet-4-6"
    assert captured_create_model["extra_kwargs"] == model_params
    assert captured["model"] is sentinel  # object, never the string


def test_build_coder_agent_wraps_create_model_failure(monkeypatch):
    def boom(spec, **kwargs):
        raise RuntimeError("no creds")

    monkeypatch.setattr(dac, "create_model", boom)

    with pytest.raises(dac.DacError) as exc_info:
        dac.build_coder_agent(get_coder_profile(model="openai_codex:gpt-5.5"), None, "/tmp")

    assert isinstance(exc_info.value.__cause__, RuntimeError)


# ---------------------------------------------------------------------------
# build_coder_agent -- trigger-lowering (Task 2)
# ---------------------------------------------------------------------------


def test_build_coder_agent_sets_model_profile_max_input_tokens_from_context_window_tokens(monkeypatch):
    # Task 2's core lever: DAC's already-installed SummarizationMiddleware
    # trigger is 0.85 x model.profile["max_input_tokens"] per round
    # (compute_summarization_defaults) -- build_coder_agent lowers it by
    # writing profile.context_window_tokens onto the built model's own
    # `.profile` dict before create_cli_agent ever sees it. Any other profile
    # keys already on the model must survive the merge untouched.
    model_stub = SimpleNamespace(profile={"max_input_tokens": 1_000_000, "supports_pdf": True})

    monkeypatch.setattr(dac, "create_model", lambda spec, **kw: SimpleNamespace(model=model_stub))
    captured: dict[str, Any] = {}
    monkeypatch.setattr(
        dac,
        "create_cli_agent",
        lambda model, aid, **kw: captured.update(model=model, kwargs=kw) or (object(), object()),
    )

    # Restrictive mode here so the assertion below actually exercises the
    # SAFETY combo -- the lowering lever is independent of the shell policy.
    profile = get_coder_profile(context_window_tokens=150_000, shell_allow_list=_SHELL_ALLOW_LIST)
    dac.build_coder_agent(profile, None, "/tmp")

    assert captured["model"] is model_stub
    assert model_stub.profile["max_input_tokens"] == 150_000
    assert model_stub.profile["supports_pdf"] is True  # untouched by the merge

    # SAFETY args are byte-for-byte the same regardless of this lever.
    kwargs = captured["kwargs"]
    assert kwargs["auto_approve"] is False
    assert kwargs["interrupt_shell_only"] is True
    assert kwargs["shell_allow_list"] == list(profile.shell_allow_list)
    assert kwargs["enable_ask_user"] is True
    assert kwargs["enable_shell"] is True


# ---------------------------------------------------------------------------
# build_coder_agent -- auto-summarizer regression guard (Task 2)
# ---------------------------------------------------------------------------


def test_summarization_middleware_is_installed_with_our_lowered_trigger(monkeypatch):
    """Regression guard for the auto-compaction spike's headline finding:
    `create_cli_agent` builds on `create_deep_agent`, which unconditionally
    installs `create_summarization_middleware(model, backend)`
    (`deepagents/graph.py`) under the public name "SummarizationMiddleware"
    -- pinning both that it's still installed and still named that after a
    `deepagents`/`deepagents-code` dependency bump.

    Spies on (never replaces) `deepagents.graph.create_summarization_middleware`
    so `dac.build_coder_agent` runs the REAL `create_model` /
    `create_cli_agent` / `create_deep_agent` -- no fake standing in for any of
    DAC's own code -- while observing exactly what gets installed and against
    which model. `ANTHROPIC_API_KEY` is set to a placeholder string purely to
    satisfy `init_chat_model`'s construction-time credential check; building
    the graph's structure never calls the model, so this makes no network
    call.
    """
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-test-dummy")
    captured: dict[str, Any] = {}

    def spy(model, backend):
        middleware = _real_create_summarization_middleware(model, backend)
        captured["model"] = model
        captured["middleware"] = middleware
        return middleware

    monkeypatch.setattr(deepagents_graph, "create_summarization_middleware", spy)

    profile = get_coder_profile(context_window_tokens=150_000)
    dac.build_coder_agent(profile, checkpointer=None, cwd=".")

    assert captured["middleware"].name == "SummarizationMiddleware"
    # Built against the SAME model instance we just lowered the trigger on --
    # proving the lever actually reaches the installed summarizer, not some
    # other, unrelated model object.
    assert captured["model"].profile["max_input_tokens"] == 150_000


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


def test_drive_turn_last_text_is_final_message_only():
    # last_text is the text of only the FINAL AIMessage (by id) in the stream,
    # so a consumer parsing a structured tail never scrapes earlier narration.
    # final_text keeps its all-text meaning (the whole turn's audit trail).
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("Reading the ticket docs now.", tokens_in=3, tokens_out=1, message_id="m1"), {})),
            ((), "messages", (ToolMessage(content="ran a search", tool_call_id="call-1"), {})),
            ((), "messages", (_ai_message("STATUS: done\nTITLE: feat: add widget", tokens_in=2, tokens_out=2, message_id="m2"), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None))

    assert result.last_text == "STATUS: done\nTITLE: feat: add widget"
    assert "Reading the ticket docs now." in result.final_text


def test_drive_turn_last_text_concatenates_parts_of_the_same_final_message():
    # Streaming delivers one logical message as several chunks sharing an id;
    # last_text must join every part of the final message, not just the last chunk.
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("earlier narration", tokens_in=1, tokens_out=1, message_id="m1"), {})),
            ((), "messages", (_ai_message("STATUS: ", tokens_in=1, tokens_out=1, message_id="m2"), {})),
            ((), "messages", (_ai_message("blocked: need a key", tokens_in=1, tokens_out=1, message_id="m2"), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None))

    assert result.last_text == "STATUS: blocked: need a key"


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


def test_drive_turn_cumulative_sum_across_rounds_does_not_trip_ceiling():
    # Each round's input is below max_tokens, but the SUM across rounds
    # exceeds it (60 + 60 + 60 = 180 > 100). The old cumulative-turn-sum
    # behavior would have tripped on round 2 -- proving the ceiling is now
    # per-round, not cumulative.
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("a", tokens_in=60, tokens_out=10), {})),
            ((), "messages", (_ai_message("b", tokens_in=60, tokens_out=10), {})),
            ((), "messages", (_ai_message("c", tokens_in=60, tokens_out=10), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is False
    assert result.outcome_hint == "completed"
    # Nothing trips early -- every chunk is consumed.
    assert len(graph.consumed) == 3
    # Reporting is still cumulative across the whole turn.
    assert result.tokens_in == 180
    assert result.tokens_out == 30


def test_drive_turn_trips_ceiling_when_single_round_input_exceeds_max_tokens():
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("a", tokens_in=40, tokens_out=10), {})),
            ((), "messages", (_ai_message("b", tokens_in=150, tokens_out=20), {})),
            ((), "messages", (_ai_message("c", tokens_in=1, tokens_out=1), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is True
    assert result.outcome_hint == "interrupted"
    # The third chunk must never be pulled once the ceiling trips.
    assert len(graph.consumed) == 2
    # Reporting still reflects cumulative in/out up to the point of stopping.
    assert result.tokens_in == 190
    assert result.tokens_out == 30


def test_drive_turn_never_trips_ceiling_when_max_tokens_is_none():
    graph = _FakeAgentGraph(
        [((), "messages", (_ai_message("a", tokens_in=10_000, tokens_out=10_000), {}))]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.token_ceiling_exceeded is False
    assert result.outcome_hint == "completed"


def test_drive_turn_ceiling_boundary_is_exceeds_not_reaches():
    # A single round landing exactly on max_tokens must not trip the
    # ceiling ("exceeds", not "reaches") -- only a strictly greater
    # single-round input does.
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("a", tokens_in=100, tokens_out=0), {})),
            ((), "messages", (_ai_message("b", tokens_in=101, tokens_out=0), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is True
    assert len(graph.consumed) == 2
    assert result.tokens_in == 201


def test_drive_turn_survives_leading_updates_chunk_with_ceiling_set():
    # Regression guard: the ceiling check lives inside the `messages` branch
    # because `turn_in` is only bound there. A stream that opens with an
    # `updates`-mode chunk (a plain state update, not an interrupt) before
    # any `messages` chunk arrives must not raise `UnboundLocalError` for
    # `turn_in` -- which is exactly what hoisting the check to run after
    # every chunk, regardless of mode, would reintroduce.
    graph = _FakeAgentGraph(
        [
            ((), "updates", {}),
            ((), "messages", (_ai_message("ok", tokens_in=10, tokens_out=5), {})),
        ]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100))

    assert result.token_ceiling_exceeded is False
    assert result.outcome_hint == "completed"
    assert result.tokens_in == 10
    assert result.tokens_out == 5


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
