"""Tests for the clipse-worker entrypoint (`clipse_agent.worker`).

worker.py's job: parse the kernel's fixed CLI flags
(`internal/spawn.workerArgs` -- --issue/--lane/--run/--thread/--workspace,
plus --checkpoint-db/--max-tokens when the kernel has them configured),
dispatch to the named lane's graph, and print exactly one line of
schema-valid `contract.WorkerResult` JSON to stdout no matter what happens
underneath -- including an unimplemented/garbage lane or an exception raised
deep inside the graph. The Coder graph itself (`clipse_agent.graphs.coder`)
is always faked here via `worker.build_coder_graph`'s own monkeypatch seam,
so these tests never touch DAC, git, or gh. The one real (but local,
network-free) piece of infrastructure exercised is the AsyncSqliteSaver
checkpointer built from --checkpoint-db, which is just a sqlite file.
"""

from __future__ import annotations

import json
import subprocess
import sys
from typing import Any

from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

from clipse_agent import worker
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult

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


def _fake_build_coder_graph(graph: _FakeGraph, build_calls: list[Any]) -> Any:
    def factory(*, checkpointer: Any = None) -> _FakeGraph:
        build_calls.append(checkpointer)
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
) -> tuple[list[str], WorkerResult]:
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(graph, build_calls))
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
    assert graph.calls[0]["config"] == {"configurable": {"thread_id": "thread-1"}}


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
# Lane dispatch: only "coder" reaches the graph
# ---------------------------------------------------------------------------


def test_unimplemented_lane_returns_blocked_transient_without_building_graph(monkeypatch, capsys):
    build_calls: list[Any] = []
    monkeypatch.setattr(worker, "build_coder_graph", _fake_build_coder_graph(_FakeGraph(), build_calls))

    exit_code = worker.main(
        ["--issue", "SPAC-2", "--lane", "reviewer", "--run", "run-1", "--thread", "t-1", "--workspace", "/ws"]
    )

    assert exit_code == 0
    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1
    result = WorkerResult.model_validate_json(lines[0])
    _assert_schema_valid(result, lines[0], blocked=True)
    assert result.block_kind == BlockKind.transient
    assert result.lane == Lane.reviewer
    assert result.run_id == "run-1"
    assert result.issue_id == "SPAC-2"
    assert result.thread_id == "t-1"
    assert "reviewer" in result.summary
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
