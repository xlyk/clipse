"""Tests for the per-ticket agent transcript writer
(`clipse_agent.transcript`). No LangGraph/DAC involved here -- these tests
only exercise the JSONL sink itself: one write failure mode (never raise),
one format guarantee (one JSON object per line, `ts` stamped at write time),
and the `bind()` context-merging contract every other module in this
feature treats as opaque."""

from __future__ import annotations

import json
from pathlib import Path

from clipse_agent.transcript import TranscriptWriter


def _read_lines(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def test_emit_appends_one_json_line_per_call(tmp_path: Path) -> None:
    writer = TranscriptWriter(tmp_path / "t.jsonl")

    writer.emit({"event": "turn_start", "task_text": "fix it"})
    writer.emit({"event": "turn_end", "outcome_hint": "completed"})

    rows = _read_lines(tmp_path / "t.jsonl")
    assert [r["event"] for r in rows] == ["turn_start", "turn_end"]
    assert rows[0]["task_text"] == "fix it"


def test_emit_stamps_ts_on_every_event(tmp_path: Path) -> None:
    writer = TranscriptWriter(tmp_path / "t.jsonl")

    writer.emit({"event": "turn_start"})

    row = _read_lines(tmp_path / "t.jsonl")[0]
    assert isinstance(row["ts"], float)
    assert row["ts"] > 0


def test_emit_creates_parent_directory(tmp_path: Path) -> None:
    path = tmp_path / "logs" / "nested" / "ISSUE-1.transcript.jsonl"
    writer = TranscriptWriter(path)

    writer.emit({"event": "turn_start"})

    assert path.exists()


def test_emit_never_raises_when_the_path_is_unwritable(tmp_path: Path, capsys) -> None:
    # tmp_path/"blocker" is a FILE, not a directory -- mkdir(parents=True)
    # for a path underneath it must fail, and emit must swallow that, not
    # propagate it: a transcript write failure must never fail a run.
    blocker = tmp_path / "blocker"
    blocker.write_text("not a directory")
    writer = TranscriptWriter(blocker / "t.jsonl")

    writer.emit({"event": "turn_start"})  # must not raise

    assert not (blocker / "t.jsonl").exists()
    assert "transcript" in capsys.readouterr().err


def test_path_property_returns_a_path(tmp_path: Path) -> None:
    target = tmp_path / "t.jsonl"
    writer = TranscriptWriter(target)

    assert writer.path == target


def test_bind_merges_context_into_every_event(tmp_path: Path) -> None:
    writer = TranscriptWriter(tmp_path / "t.jsonl")
    sink = writer.bind(lane="coder", run_id="run-1", thread_id="thread-1")

    sink({"event": "assistant", "text": "hello"})
    sink({"event": "tool_call", "name": "shell", "args": {"cmd": "ls"}})

    rows = _read_lines(tmp_path / "t.jsonl")
    for row in rows:
        assert row["lane"] == "coder"
        assert row["run_id"] == "run-1"
        assert row["thread_id"] == "thread-1"
    assert rows[0] == {**rows[0], "event": "assistant", "text": "hello"}
    assert rows[1]["name"] == "shell"
    assert rows[1]["args"] == {"cmd": "ls"}


def test_bind_returned_sink_is_reusable_across_many_events(tmp_path: Path) -> None:
    writer = TranscriptWriter(tmp_path / "t.jsonl")
    sink = writer.bind(lane="reviewer")

    for i in range(5):
        sink({"event": "assistant", "text": f"part {i}"})

    rows = _read_lines(tmp_path / "t.jsonl")
    assert len(rows) == 5
    assert [r["text"] for r in rows] == [f"part {i}" for i in range(5)]
