"""Tests for the clipse-worker entrypoint (`clipse_agent.worker`).

worker.py's job: parse the kernel's fixed CLI flags
(`internal/spawn.workerArgs` -- --issue/--lane/--run/--thread/--workspace,
plus --checkpoint-db/--max-tokens when the kernel has them configured),
dispatch to the named lane's graph, and print exactly one line of
schema-valid `contract.WorkerResult` JSON to stdout no matter what happens
underneath -- including an unimplemented/garbage lane or an exception raised
deep inside the graph. Every wired lane's graph (`clipse_agent.graphs.
{coder,reviewer}`) is always faked here via `worker.build_coder_graph`
/ `worker.build_reviewer_graph`'s own monkeypatch seams, so these tests never
touch DAC, git, or gh. The one real
(but local, network-free) piece of infrastructure exercised is the
AsyncSqliteSaver checkpointer built from --checkpoint-db, which is just a
sqlite file.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path
from types import SimpleNamespace
from typing import Any

import pytest
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver
from daytona import DaytonaAuthenticationError, DaytonaNotFoundError, DaytonaTimeoutError, DaytonaValidationError

from clipse_agent import worker
from clipse_agent.backends.contracts import (
    BackendActionError,
    BackendActionRequest,
    BackendActionResult,
    BackendWorkspace,
)
from clipse_agent.backends.daytona import (
    REMOTE_REPO_ABS,
    DaytonaSession,
    RepositoryScopedDaytonaSandbox,
)
from clipse_agent.backends.local import LocalSession
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.transcript import TranscriptWriter

# ---------------------------------------------------------------------------
# Fakes
# ---------------------------------------------------------------------------


class _FakeGraph:
    """Stand-in for the compiled graph `build_coder_graph` returns.

    Records every `ainvoke` call and either returns a canned final state or
    raises a canned exception -- so a test can drive both the happy path
    and worker.py's catch-all without ever building a real Coder graph.
    """

    def __init__(
        self,
        final_state: dict[str, Any] | None = None,
        raises: BaseException | None = None,
    ) -> None:
        self._final_state = final_state
        self._raises = raises
        self.calls: list[dict[str, Any]] = []

    async def ainvoke(self, input_state: dict[str, Any], config: dict[str, Any]) -> dict[str, Any]:
        self.calls.append({"input_state": input_state, "config": config})
        if self._raises is not None:
            raise self._raises
        assert self._final_state is not None
        return self._final_state


def _fake_build_coder_graph(
    graph: _FakeGraph, build_calls: list[Any], kwarg_calls: list[dict[str, Any]] | None = None
) -> Any:
    """Named after its original (Coder-lane) use, but genuinely lane-generic:
    every `build_*_graph` function worker.py calls takes the same
    `checkpointer=...` keyword and returns a compiled graph, so this one
    fake factory is reused below for `build_reviewer_graph` too -- mirrors
    `dac.build_coder_agent` being reused across lanes for the same reason
    (see graphs/reviewer.py's docstring).

    Accepts `**kwargs` -- not just `checkpointer` -- because `_dispatch` now
    forwards lane-specific `extra_kwargs` (e.g. `profile`/`docs_profile`)
    into `build_graph` alongside `checkpointer`; every call's kwargs are
    recorded into `kwarg_calls` (when given) so a test can assert on exactly
    what `_dispatch` passed through.
    """

    def factory(*, checkpointer: Any = None, **kwargs: Any) -> _FakeGraph:
        build_calls.append(checkpointer)
        if kwarg_calls is not None:
            kwarg_calls.append(kwargs)
        return graph

    return factory


def _canned_result(**overrides: Any) -> WorkerResult:
    fields: dict[str, Any] = {
        "run_id": "run-1",
        "issue_id": "SPAC-1",
        "lane": Lane.coder,
        "outcome": Outcome.needs_review,
        "summary": "opened a PR",
        "artifacts": ["src/x.py"],
        "pr_url": "https://github.com/acme/widgets/pull/1",
        "thread_id": "thread-1",
        "turn_count": 1,
        "tokens": Tokens(**{"in": 10, "out": 20}),
    }
    fields.update(overrides)
    return WorkerResult(**fields)


def _run_main_capture(
    monkeypatch: Any,
    capsys: Any,
    argv: list[str],
    *,
    graph: _FakeGraph,
    build_calls: list[Any],
    attr: str = "build_coder_graph",
    kwarg_calls: list[dict[str, Any]] | None = None,
) -> tuple[list[str], WorkerResult]:
    monkeypatch.setattr(worker, attr, _fake_build_coder_graph(graph, build_calls, kwarg_calls))
    exit_code = worker.main(argv)
    assert exit_code == 0

    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1, f"expected exactly one stdout line, got {lines!r}"

    result = WorkerResult.model_validate_json(lines[0])
    return lines, result


def _assert_schema_valid(result: WorkerResult, raw_line: str, *, blocked: bool) -> None:
    """Every result worker.py prints must round-trip through the generated
    model, and block_kind must be present iff outcome == blocked (amendment
    X2) -- both on the object and in the raw dumped JSON.
    """
    raw = json.loads(raw_line)
    reparsed = WorkerResult.model_validate_json(raw_line)
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
# Happy path: dispatches to the Coder graph, prints its result verbatim
# ---------------------------------------------------------------------------


def test_main_dispatches_coder_lane_and_prints_graph_result(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    lines, result = _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue",
            "SPAC-1",
            "--lane",
            "coder",
            "--run",
            "run-1",
            "--thread",
            "thread-1",
            "--workspace",
            "/ws",
        ],
        graph=graph,
        build_calls=build_calls,
    )

    assert result == canned
    _assert_schema_valid(result, lines[0], blocked=False)

    # No --checkpoint-db given => built with no checkpointer at all.
    assert build_calls == [None]
    assert len(graph.calls) == 1
    input_state = graph.calls[0]["input_state"]
    assert input_state["issue_id"] == "SPAC-1"
    assert input_state["run_id"] == "run-1"
    assert input_state["thread_id"] == "thread-1"
    assert input_state["workspace"] == "/ws"
    assert input_state["max_tokens"] is None
    # The outer wrapping-graph checkpoint thread_id is namespaced by lane
    # (see test_outer_thread_id_is_namespaced_per_lane_and_distinct_across_lanes)
    # -- WorkerResult.thread_id itself (input_state["thread_id"], asserted
    # above) stays the raw, un-namespaced value.
    assert graph.calls[0]["config"] == {"configurable": {"thread_id": "thread-1::coder"}}


def test_main_defaults_lane_to_coder_when_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue", "SPAC-1", "--run", "run-1", "--thread", "thread-1", "--workspace", "/ws"],
        graph=graph,
        build_calls=build_calls,
    )

    assert len(graph.calls) == 1  # the default lane dispatched straight to the coder graph


def test_main_accepts_equals_form_flags_like_the_kernel_spawner(monkeypatch, capsys):
    # internal/spawn.workerArgs builds flags as "--issue=" + value (see
    # internal/spawn/local.go's workerArgs), never space-separated. argparse
    # accepts both forms, but pin the exact shape the kernel actually uses.
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-9",
            "--lane=coder",
            "--run=run-9",
            "--thread=thread-9",
            "--workspace=/ws9",
            "--max-tokens=12345",
        ],
        graph=graph,
        build_calls=build_calls,
    )

    input_state = graph.calls[0]["input_state"]
    assert input_state["issue_id"] == "SPAC-9"
    assert input_state["max_tokens"] == 12345


# ---------------------------------------------------------------------------
# --base-branch threading: internal/spawn.workerArgs appends --base-branch=<v>
# from cfg.Repo.BaseBranch whenever it's non-empty (Task 1); the coder graph's
# sync_base node reads it from state["base_branch"] (Task 2), so it must flow
# from the flag into input_state exactly like --workspace does.
# ---------------------------------------------------------------------------


def test_dispatch_threads_base_branch_into_input_state(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            "--base-branch=main",
        ],
        graph=graph,
        build_calls=build_calls,
    )

    assert graph.calls[0]["input_state"]["base_branch"] == "main"


def test_dispatch_base_branch_defaults_to_empty_string_when_flag_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-1", "--lane=coder", "--run=run-1", "--thread=thread-1", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
    )

    assert graph.calls[0]["input_state"]["base_branch"] == ""


def test_dispatch_builds_local_session_and_preserves_local_agent_factory_shape(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    kwarg_calls: list[dict[str, Any]] = []
    dac_calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []
    monkeypatch.setattr(
        worker.dac,
        "build_coder_agent",
        lambda *args, **kwargs: dac_calls.append((args, kwargs)) or (object(), object()),
    )

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--lane=coder", "--workspace=/ws", "--repo-slug=xlyk/clipse"],
        graph=graph,
        build_calls=[],
        kwarg_calls=kwarg_calls,
    )

    session = kwarg_calls[-1]["session"]
    assert isinstance(session, LocalSession)
    assert session.cwd == "/ws"
    assert session.repo_slug == "xlyk/clipse"
    profile = object()
    kwarg_calls[-1]["agent_factory"](profile, None, "/ws")
    assert dac_calls == [((profile, None, "/ws"), {})]


@pytest.mark.parametrize(
    ("lane", "graph_attr"),
    [("coder", "build_coder_graph"), ("reviewer", "build_reviewer_graph")],
)
def test_dispatch_builds_daytona_session_for_each_graph(
    monkeypatch, capsys, lane: str, graph_attr: str
) -> None:
    raw_sandbox = SimpleNamespace(git=SimpleNamespace())
    backend = object()
    client = SimpleNamespace(get=lambda sandbox_id: raw_sandbox)
    monkeypatch.setattr(worker, "Daytona", lambda: client)
    monkeypatch.setattr(worker, "DaytonaSandbox", lambda *, sandbox: backend)
    dac_calls: list[tuple[tuple[Any, ...], dict[str, Any]]] = []
    monkeypatch.setattr(
        worker.dac,
        "build_coder_agent",
        lambda *args, **kwargs: dac_calls.append((args, kwargs)) or (object(), object()),
    )
    lane_value = Lane(lane)
    canned = _canned_result(
        lane=lane_value,
        outcome=Outcome.needs_review if lane_value == Lane.coder else Outcome.done,
    )
    graph = _FakeGraph(final_state={"result": canned})
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            f"--lane={lane}",
            "--workspace=/home/daytona/workspace/clipse",
            "--backend=daytona",
            "--sandbox-id=sandbox-1",
            "--repo-slug=xlyk/clipse",
        ],
        graph=graph,
        build_calls=[],
        attr=graph_attr,
        kwarg_calls=kwarg_calls,
    )

    session = kwarg_calls[-1]["session"]
    assert isinstance(session, DaytonaSession)
    assert isinstance(session.sandbox, RepositoryScopedDaytonaSandbox)
    assert session.sandbox.backend is backend
    profile = worker.get_coder_profile() if lane_value == Lane.coder else worker.get_reviewer_profile()
    kwarg_calls[-1]["agent_factory"](profile, None, session.cwd)
    built_profile = dac_calls[0][0][0]
    assert REMOTE_REPO_ABS in built_profile.system_prompt
    assert "repository root" in built_profile.system_prompt.lower()
    assert dac_calls[0][1] == {"sandbox": session.sandbox, "sandbox_type": "daytona"}


@pytest.mark.parametrize("failure_at", ["get", "attach"])
def test_daytona_session_attach_failure_is_sanitized_transient(
    monkeypatch, capsys, failure_at: str
) -> None:
    canary = "provider-body-ghp_attach_canary"
    raw_sandbox = SimpleNamespace(git=SimpleNamespace())

    def get(_sandbox_id: str) -> object:
        if failure_at == "get":
            raise RuntimeError(canary)
        return raw_sandbox

    def attach(*, sandbox: object) -> object:
        assert sandbox is raw_sandbox
        if failure_at == "attach":
            raise RuntimeError(canary)
        return object()

    monkeypatch.setattr(worker, "Daytona", lambda: SimpleNamespace(get=get))
    monkeypatch.setattr(worker, "DaytonaSandbox", attach)

    exit_code = worker.main(
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            f"--workspace={REMOTE_REPO_ABS}",
            "--backend=daytona",
            "--sandbox-id=sandbox-1",
            "--repo-slug=xlyk/clipse",
        ]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    assert captured.err == ""
    assert canary not in captured.out
    result = WorkerResult.model_validate_json(captured.out)
    assert result.outcome == Outcome.blocked
    assert result.block_kind == BlockKind.transient
    assert result.summary == "clipse-worker: Daytona sandbox attachment failed"
    assert canary not in result.model_dump_json()


# ---------------------------------------------------------------------------
# --max-tokens / CLIPSE_MAX_TOKENS resolution
# ---------------------------------------------------------------------------


def test_main_falls_back_to_env_max_tokens_when_flag_omitted(monkeypatch, capsys):
    monkeypatch.setenv("CLIPSE_MAX_TOKENS", "999")
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(monkeypatch, capsys, ["--workspace", "/ws"], graph=graph, build_calls=build_calls)

    assert graph.calls[0]["input_state"]["max_tokens"] == 999


def test_main_flag_max_tokens_wins_over_env(monkeypatch, capsys):
    monkeypatch.setenv("CLIPSE_MAX_TOKENS", "999")
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--workspace", "/ws", "--max-tokens", "42"],
        graph=graph,
        build_calls=build_calls,
    )

    assert graph.calls[0]["input_state"]["max_tokens"] == 42


def test_main_ignores_malformed_env_max_tokens(monkeypatch, capsys):
    monkeypatch.setenv("CLIPSE_MAX_TOKENS", "not-an-int")
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(monkeypatch, capsys, ["--workspace", "/ws"], graph=graph, build_calls=build_calls)

    assert graph.calls[0]["input_state"]["max_tokens"] is None


def test_main_no_ceiling_when_neither_flag_nor_env_set(monkeypatch, capsys):
    monkeypatch.delenv("CLIPSE_MAX_TOKENS", raising=False)
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(monkeypatch, capsys, ["--workspace", "/ws"], graph=graph, build_calls=build_calls)

    assert graph.calls[0]["input_state"]["max_tokens"] is None


# ---------------------------------------------------------------------------
# --checkpoint-db wiring (real AsyncSqliteSaver -- local sqlite file only)
# ---------------------------------------------------------------------------


def test_main_builds_real_asyncsqlitesaver_checkpointer_from_checkpoint_db_flag(monkeypatch, capsys, tmp_path):
    db_path = tmp_path / "SPAC-1.db"
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--workspace", "/ws", "--checkpoint-db", str(db_path)],
        graph=graph,
        build_calls=build_calls,
    )

    assert len(build_calls) == 1
    assert isinstance(build_calls[0], AsyncSqliteSaver)
    assert db_path.exists()


def test_main_uses_no_checkpointer_when_checkpoint_db_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    _run_main_capture(monkeypatch, capsys, ["--workspace", "/ws"], graph=graph, build_calls=build_calls)

    assert build_calls == [None]


# ---------------------------------------------------------------------------
# Lane dispatch: coder/reviewer each reach their own graph; anything
# else (including the real Lane member "git_operator", which never gets a
# Python graph -- decision O/J amendment: it runs as deterministic Go
# internal/gitops -- and any garbage string) stays blocked/transient.
# ---------------------------------------------------------------------------


def test_main_dispatches_reviewer_lane_and_prints_graph_result(monkeypatch, capsys):
    canned = _canned_result(
        lane=Lane.reviewer,
        outcome=Outcome.done,
        summary="reviewed the diff: no issues found",
    )
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []

    lines, result = _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue",
            "SPAC-20",
            "--lane",
            "reviewer",
            "--run",
            "run-1",
            "--thread",
            "thread-20",
            "--workspace",
            "/ws",
        ],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
    )

    assert result == canned
    _assert_schema_valid(result, lines[0], blocked=False)

    # No --checkpoint-db given => built with no checkpointer at all.
    assert build_calls == [None]
    assert len(graph.calls) == 1
    input_state = graph.calls[0]["input_state"]
    assert input_state["issue_id"] == "SPAC-20"
    assert input_state["run_id"] == "run-1"
    assert input_state["thread_id"] == "thread-20"
    assert input_state["workspace"] == "/ws"
    assert input_state["max_tokens"] is None
    assert graph.calls[0]["config"] == {"configurable": {"thread_id": "thread-20::reviewer"}}


# ---------------------------------------------------------------------------
# --model/--docs-model threading: _dispatch resolves these into the lane's
# profile(s) and forwards them to build_graph as extra kwargs.
# ---------------------------------------------------------------------------


def test_dispatch_threads_model_into_coder_profile(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            "--model=openai_codex:gpt-5.5",
            "--docs-model=openai_codex:gpt-5.4",
        ],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model == "openai_codex:gpt-5.5"
    assert kwarg_calls[-1]["docs_profile"].model == "openai_codex:gpt-5.4"


def test_dispatch_coder_falls_back_to_default_models_when_flags_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-1", "--lane=coder", "--run=run-1", "--thread=thread-1", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model == "anthropic:claude-sonnet-4-6"
    assert kwarg_calls[-1]["docs_profile"].model == "anthropic:claude-sonnet-4-6"


def test_dispatch_reviewer_has_no_docs_profile(monkeypatch, capsys):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-20",
            "--lane=reviewer",
            "--run=run-1",
            "--thread=thread-20",
            "--workspace=/ws",
            "--model=anthropic:claude-opus-4-6",
        ],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model == "anthropic:claude-opus-4-6"
    assert "docs_profile" not in kwarg_calls[-1]


# ---------------------------------------------------------------------------
# --model-params/--docs-model-params threading: same shape as --model/
# --docs-model above, but each carries a JSON-encoded dict of extra
# model-construction kwargs instead of a provider:model spec string.
# ---------------------------------------------------------------------------


def test_dispatch_threads_model_params_into_coder_and_docs_profile(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            '--model-params={"reasoning_effort": "high"}',
            '--docs-model-params={"reasoning_effort": "low"}',
        ],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model_params == {"reasoning_effort": "high"}
    assert kwarg_calls[-1]["docs_profile"].model_params == {"reasoning_effort": "low"}


def test_dispatch_coder_model_params_default_to_none_when_flags_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-1", "--lane=coder", "--run=run-1", "--thread=thread-1", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model_params is None
    assert kwarg_calls[-1]["docs_profile"].model_params is None


def test_dispatch_threads_model_params_into_reviewer_profile(monkeypatch, capsys):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-20",
            "--lane=reviewer",
            "--run=run-1",
            "--thread=thread-20",
            "--workspace=/ws",
            '--model-params={"reasoning_effort": "high"}',
        ],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model_params == {"reasoning_effort": "high"}


def test_dispatch_reviewer_model_params_default_to_none_when_flag_omitted(monkeypatch, capsys):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-20", "--lane=reviewer", "--run=run-1", "--thread=thread-20", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].model_params is None


# ---------------------------------------------------------------------------
# --shell-allow-list/--docs-shell-allow-list threading: absent flag means the
# `all` policy (unrestricted shell) -- the kernel omits the flag entirely for
# an all-policy lane (internal/spawn.workerArgs) -- present flag carries a
# JSON array that becomes the profile's restrictive tuple.
# ---------------------------------------------------------------------------


def test_shell_allow_list_flag_reaches_coder_profile(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            '--shell-allow-list=["git","gh"]',
        ],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].shell_allow_list == ("git", "gh")
    # --docs-shell-allow-list was omitted -> all (unrestricted).
    assert kwarg_calls[-1]["docs_profile"].shell_allow_list is None


def test_docs_shell_allow_list_flag_reaches_docs_profile_only(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            '--docs-shell-allow-list=["git","gh","ls"]',
        ],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    # --shell-allow-list was omitted -> all (unrestricted) for the coding turn.
    assert kwarg_calls[-1]["profile"].shell_allow_list is None
    assert kwarg_calls[-1]["docs_profile"].shell_allow_list == ("git", "gh", "ls")


def test_dispatch_coder_shell_allow_list_defaults_to_none_when_flags_omitted(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-1", "--lane=coder", "--run=run-1", "--thread=thread-1", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].shell_allow_list is None
    assert kwarg_calls[-1]["docs_profile"].shell_allow_list is None


def test_dispatch_threads_shell_allow_list_into_reviewer_profile(monkeypatch, capsys):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-20",
            "--lane=reviewer",
            "--run=run-1",
            "--thread=thread-20",
            "--workspace=/ws",
            '--shell-allow-list=["git","gh","cat"]',
        ],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].shell_allow_list == ("git", "gh", "cat")


def test_dispatch_reviewer_shell_allow_list_defaults_to_none_when_flag_omitted(monkeypatch, capsys):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-20", "--lane=reviewer", "--run=run-1", "--thread=thread-20", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["profile"].shell_allow_list is None


# ---------------------------------------------------------------------------
# --transcript threading: absent flag (the default "") means disabled --
# _dispatch's extra_kwargs carries transcript=None, same "no config means
# None" contract as --model-params/--shell-allow-list above. A present flag
# carries the jsonl path a TranscriptWriter is built from.
# ---------------------------------------------------------------------------


def test_transcript_flag_reaches_coder_and_docs_extra_kwargs(monkeypatch, capsys, tmp_path):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []
    transcript_path = str(tmp_path / "SPAC-1.transcript.jsonl")

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=coder",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            f"--transcript={transcript_path}",
        ],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    transcript = kwarg_calls[-1]["transcript"]
    assert isinstance(transcript, TranscriptWriter)
    assert transcript.path == Path(transcript_path)


def test_transcript_omitted_flag_disables_transcript_for_coder(monkeypatch, capsys):
    canned = _canned_result()
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []

    _run_main_capture(
        monkeypatch,
        capsys,
        ["--issue=SPAC-1", "--lane=coder", "--run=run-1", "--thread=thread-1", "--workspace=/ws"],
        graph=graph,
        build_calls=build_calls,
        kwarg_calls=kwarg_calls,
    )

    assert kwarg_calls[-1]["transcript"] is None


def test_transcript_flag_reaches_reviewer_extra_kwargs(monkeypatch, capsys, tmp_path):
    canned = _canned_result(lane=Lane.reviewer, outcome=Outcome.done, summary="reviewed the diff: no issues found")
    graph = _FakeGraph(final_state={"result": canned})
    build_calls: list[Any] = []
    kwarg_calls: list[dict[str, Any]] = []
    transcript_path = str(tmp_path / "SPAC-1.transcript.jsonl")

    _run_main_capture(
        monkeypatch,
        capsys,
        [
            "--issue=SPAC-1",
            "--lane=reviewer",
            "--run=run-1",
            "--thread=thread-1",
            "--workspace=/ws",
            f"--transcript={transcript_path}",
        ],
        graph=graph,
        build_calls=build_calls,
        attr="build_reviewer_graph",
        kwarg_calls=kwarg_calls,
    )

    transcript = kwarg_calls[-1]["transcript"]
    assert isinstance(transcript, TranscriptWriter)
    assert transcript.path == Path(transcript_path)


# ---------------------------------------------------------------------------
# Outer wrapping-graph checkpoint thread_id: namespaced per lane so
# coder/reviewer never collide on the same physical checkpoint (the inner DAC
# thread is already namespaced this way one level down -- see graphs/coder.py's
# "::dac" and the reviewer analogue -- but the OUTER wrapping graph this module
# drives was not, before this fix).
# ---------------------------------------------------------------------------


def test_outer_thread_id_is_namespaced_per_lane_and_distinct_across_lanes(monkeypatch, capsys):
    # A fresh claim's --thread is the SAME raw value regardless of lane (the
    # kernel has no reason to vary it by lane -- see internal/spawn's
    # workerArgs) -- so without a per-lane namespace here, coder/reviewer
    # dispatched against the same issue would collide on the OUTER
    # wrapping-graph's own checkpoint thread_id.
    shared_thread = "shared-thread"

    coder_graph = _FakeGraph(final_state={"result": _canned_result()})
    _run_main_capture(
        monkeypatch,
        capsys,
        ["--lane", "coder", "--run", "run-1", "--thread", shared_thread, "--workspace", "/ws"],
        graph=coder_graph,
        build_calls=[],
    )

    reviewer_graph = _FakeGraph(final_state={"result": _canned_result(lane=Lane.reviewer, outcome=Outcome.done)})
    _run_main_capture(
        monkeypatch,
        capsys,
        ["--lane", "reviewer", "--run", "run-1", "--thread", shared_thread, "--workspace", "/ws"],
        graph=reviewer_graph,
        build_calls=[],
        attr="build_reviewer_graph",
    )

    coder_thread = coder_graph.calls[0]["config"]["configurable"]["thread_id"]
    reviewer_thread = reviewer_graph.calls[0]["config"]["configurable"]["thread_id"]

    assert coder_thread == "shared-thread::coder"
    assert reviewer_thread == "shared-thread::reviewer"
    assert coder_thread != reviewer_thread


def test_unimplemented_lane_returns_blocked_transient_without_building_graph(monkeypatch, capsys):
    # git_operator is a real Lane member that intentionally never gets a
    # Python graph -- it runs as deterministic Go (internal/gitops), not a
    # DAC worker -- so it is the one lane guaranteed to stay unimplemented
    # here for as long as that design decision holds.
    build_calls: list[Any] = []
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(_FakeGraph(), build_calls))

    exit_code = worker.main(
        ["--issue", "SPAC-2", "--lane", "git_operator", "--run", "run-1", "--thread", "t-1", "--workspace", "/ws"]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert result.lane == Lane.git_operator
    assert result.run_id == "run-1"
    assert result.issue_id == "SPAC-2"
    assert result.thread_id == "t-1"
    assert "git_operator" in result.summary
    assert build_calls == []  # never touched the coder graph


def test_garbage_lane_string_returns_blocked_transient_without_crashing(monkeypatch, capsys):
    build_calls: list[Any] = []
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(_FakeGraph(), build_calls))

    exit_code = worker.main(
        ["--issue", "SPAC-3", "--lane", "not-a-real-lane", "--run", "run-1", "--thread", "t-1", "--workspace", "/ws"]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    # A string that isn't a real Lane member can't be echoed back into a
    # strict WorkerResult (extra='forbid' on the model doesn't relax enum
    # membership) -- it must fall back to a safe, valid Lane instead.
    assert result.lane == Lane.coder
    assert build_calls == []


# ---------------------------------------------------------------------------
# Catch-all: any exception deep in the graph still yields one valid result
# ---------------------------------------------------------------------------


def test_graph_exception_produces_blocked_transient_result(monkeypatch, capsys):
    build_calls: list[Any] = []
    graph = _FakeGraph(raises=RuntimeError("boom from the graph"))
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(graph, build_calls))

    exit_code = worker.main(
        ["--issue", "SPAC-4", "--lane", "coder", "--run", "run-9", "--thread", "t-9", "--workspace", "/ws"]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert result.run_id == "run-9"
    assert result.issue_id == "SPAC-4"
    assert result.thread_id == "t-9"
    assert "boom from the graph" in result.summary

    # Diagnostics go to stderr, never stdout -- stdout carries only the one
    # JSON line asserted above.
    assert "boom from the graph" in captured.err
    assert "Traceback" in captured.err


def test_graph_exception_in_reviewer_lane_preserves_lane_in_blocked_result(monkeypatch, capsys):
    # main()'s top-level except must report whichever lane was actually
    # requested (mirroring _dispatch's own fallback at line ~166), not
    # hardcode Lane.coder regardless of --lane.
    build_calls: list[Any] = []
    graph = _FakeGraph(raises=RuntimeError("boom from the reviewer graph"))
    monkeypatch.setattr(worker, "build_reviewer_graph", _fake_build_coder_graph(graph, build_calls))

    exit_code = worker.main(
        ["--issue", "SPAC-4b", "--lane", "reviewer", "--run", "run-9", "--thread", "t-9", "--workspace", "/ws"]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert result.lane == Lane.reviewer


def test_checkpointer_construction_failure_also_yields_blocked_transient(monkeypatch, capsys, tmp_path):
    # A bad --checkpoint-db path (e.g. an unwritable directory) must be as
    # safe as any other internal error -- the graph is never even reached.
    build_calls: list[Any] = []
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(_FakeGraph(), build_calls))
    bad_path = tmp_path / "does" / "not" / "exist" / "ckpt.db"

    exit_code = worker.main(["--issue", "SPAC-5", "--workspace", "/ws", "--checkpoint-db", str(bad_path)])

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert build_calls == []


# ---------------------------------------------------------------------------
# Real (unmocked) path: a genuinely missing workspace fails safe end to end.
# Never reaches DAC -- ensure_worktree raises before run_DAC is scheduled --
# so this stays LLM-free and network-free while proving the catch-all covers
# a real failure, not just a mocked one.
# ---------------------------------------------------------------------------


def test_main_tolerates_missing_args_via_the_real_graph(capsys):
    exit_code = worker.main([])

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert result.lane == Lane.coder


# ---------------------------------------------------------------------------
# handoff: optional WorkerResult field (Task 17). Omitted from the dumped JSON
# when None, present when set -- exactly how the dispatcher decides whether to
# post a handoff Linear comment (Task 19).
# ---------------------------------------------------------------------------


def test_worker_result_handoff_omitted_when_none():
    result = _canned_result()
    assert result.handoff is None
    dumped = json.loads(result.model_dump_json(exclude_none=True))
    assert "handoff" not in dumped


def test_worker_result_handoff_included_when_set():
    result = _canned_result(handoff="- chose drop semantics\n- added Widget.build")
    dumped = json.loads(result.model_dump_json(exclude_none=True))
    assert dumped["handoff"] == "- chose drop semantics\n- added Widget.build"


def test_clipse_worker_module_emits_exactly_one_valid_json_line_with_no_args():
    proc = subprocess.run(
        [sys.executable, "-m", "clipse_agent.worker"],
        capture_output=True,
        text=True,
        check=True,
    )
    lines = proc.stdout.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    assert result.outcome == Outcome.blocked
    assert result.block_kind == BlockKind.transient


# ---------------------------------------------------------------------------
# Backend lifecycle mode: typed JSON before any lane parsing or graph work.
# ---------------------------------------------------------------------------


def _backend_argv(action: str = "ensure", provider: str = "daytona") -> list[str]:
    return [
        f"--backend-action={action}",
        f"--backend-provider={provider}",
        "--backend-role=coder",
        "--repo-url=https://github.com/xlyk/clipse.git",
        "--repo-slug=xlyk/clipse",
        "--base-branch=main",
        "--branch=feat/CLI-1",
        "--issue=issue-1",
        "--run=run-1",
        "--auto-stop-minutes=60",
        "--reviewer-auto-delete-minutes=45",
        "--snapshot=clipse-snapshot",
        "--target=us",
    ]


class _FakeLifecycle:
    def __init__(self, *, raises: BaseException | None = None) -> None:
        self.raises = raises
        self.calls: list[tuple[str, BackendActionRequest]] = []

    def _record(self, action: str, request: BackendActionRequest) -> None:
        self.calls.append((action, request))
        if self.raises is not None:
            raise self.raises

    def ensure(self, request: BackendActionRequest) -> BackendWorkspace:
        self._record("ensure", request)
        return BackendWorkspace(
            external_id="sandbox-1",
            state="active",
            workspace_path="/home/daytona/workspace/clipse",
            owner_key="daytona:xlyk/clipse:coder:issue-1",
        )

    def delete(self, request: BackendActionRequest) -> BackendWorkspace:
        self._record("delete", request)
        return BackendWorkspace(
            external_id=request.sandbox_id or "sandbox-1",
            state="deleted",
            workspace_path="/home/daytona/workspace/clipse",
            owner_key="daytona:xlyk/clipse:coder:issue-1",
        )

    def list(self, request: BackendActionRequest) -> list[BackendWorkspace]:
        self._record("list", request)
        return [
            BackendWorkspace(
                external_id="sandbox-1",
                state="stopped",
                workspace_path="/home/daytona/workspace/clipse",
                owner_key="daytona:xlyk/clipse:coder:issue-1",
            )
        ]


def _run_backend_main(monkeypatch, capsys, lifecycle: _FakeLifecycle, argv: list[str]) -> BackendActionResult:
    monkeypatch.setattr(worker, "DaytonaLifecycle", lambda: lifecycle)

    assert worker.main(argv) == 0

    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    return BackendActionResult.model_validate_json(lines[0])


def test_backend_action_ensure_prints_one_typed_success_before_lane_dispatch(monkeypatch, capsys) -> None:
    lifecycle = _FakeLifecycle()
    monkeypatch.setattr(worker, "build_coder_graph", lambda **_kwargs: pytest.fail("lane graph must not build"))

    result = _run_backend_main(monkeypatch, capsys, lifecycle, _backend_argv())

    assert result.ok is True
    assert result.external_id == "sandbox-1"
    assert result.workspace_path == "/home/daytona/workspace/clipse"
    assert result.owner_key == "daytona:xlyk/clipse:coder:issue-1"
    assert result.state == "active"
    assert result.error_kind is None
    assert lifecycle.calls[0][0] == "ensure"
    request = lifecycle.calls[0][1]
    assert request.target == "us"
    assert request.snapshot == "clipse-snapshot"


def test_backend_action_list_prints_typed_workspace_list(monkeypatch, capsys) -> None:
    lifecycle = _FakeLifecycle()

    result = _run_backend_main(monkeypatch, capsys, lifecycle, _backend_argv("list"))

    assert result.ok is True
    assert result.external_id is None
    assert result.workspaces is not None
    assert [item.external_id for item in result.workspaces] == ["sandbox-1"]


def test_backend_action_failure_is_typed_and_still_exits_zero(monkeypatch, capsys) -> None:
    lifecycle = _FakeLifecycle(raises=BackendActionError("needs_input", "ensure", "authenticate GitHub first"))

    result = _run_backend_main(monkeypatch, capsys, lifecycle, _backend_argv())

    assert result.ok is False
    assert result.error_kind == "needs_input"
    assert result.error_operation == "ensure"
    assert result.error == "authenticate GitHub first"


@pytest.mark.parametrize("exc", [DaytonaAuthenticationError("no API key"), BackendActionError("needs_input", "github_auth", "no auth")])
def test_backend_action_maps_missing_api_or_github_auth_to_needs_input(monkeypatch, capsys, exc) -> None:
    result = _run_backend_main(monkeypatch, capsys, _FakeLifecycle(raises=exc), _backend_argv())

    assert result.ok is False
    assert result.error_kind == "needs_input"


def test_backend_action_maps_provider_timeout_to_transient(monkeypatch, capsys) -> None:
    token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
    lifecycle = _FakeLifecycle(raises=DaytonaTimeoutError(f"timed out with {token}"))

    result = _run_backend_main(monkeypatch, capsys, lifecycle, _backend_argv())

    assert result.ok is False
    assert result.error_kind == "transient"
    assert result.error_operation == "daytona"
    assert token not in (result.error or "")


@pytest.mark.parametrize(
    ("action", "provider"),
    [("explode", "daytona"), ("ensure", "not-supported")],
)
def test_backend_action_maps_unsupported_provider_or_action_to_capability(
    monkeypatch, capsys, action: str, provider: str
) -> None:
    result = _run_backend_main(monkeypatch, capsys, _FakeLifecycle(), _backend_argv(action, provider))

    assert result.ok is False
    assert result.error_kind == "capability"
    assert result.error_operation == "backend_action"


def test_backend_action_maps_invalid_configuration_to_needs_input(monkeypatch, capsys) -> None:
    result = _run_backend_main(
        monkeypatch,
        capsys,
        _FakeLifecycle(raises=DaytonaValidationError("invalid target")),
        _backend_argv(),
    )

    assert result.ok is False
    assert result.error_kind == "needs_input"
    assert result.error_operation == "daytona_config"


def test_backend_action_maps_unscoped_not_found_to_transient(monkeypatch, capsys) -> None:
    result = _run_backend_main(
        monkeypatch,
        capsys,
        _FakeLifecycle(raises=DaytonaNotFoundError("sandbox vanished")),
        _backend_argv(),
    )

    assert result.ok is False
    assert result.error_kind == "transient"
    assert result.error_operation == "daytona"


@pytest.mark.parametrize("exc", [ImportError("missing Daytona symbol"), AttributeError("SDK method missing")])
def test_backend_action_maps_sdk_or_dependency_incompatibility_to_capability(monkeypatch, capsys, exc) -> None:
    result = _run_backend_main(monkeypatch, capsys, _FakeLifecycle(raises=exc), _backend_argv())

    assert result.ok is False
    assert result.error_kind == "capability"
    assert result.error_operation == "daytona_sdk"


def test_backend_action_list_accepts_repo_identity_without_issue_scope(monkeypatch, capsys) -> None:
    lifecycle = _FakeLifecycle()

    result = _run_backend_main(
        monkeypatch,
        capsys,
        lifecycle,
        ["--backend-action=list", "--backend-provider=daytona", "--repo-slug=xlyk/clipse"],
    )

    assert result.ok is True
    request = lifecycle.calls[0][1]
    assert request.issue_id is None
    assert request.run_id is None
    assert request.role is None
