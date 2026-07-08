# Feature D: Per-Ticket Agent Transcript Logging — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every DAC turn (coder, coder_docs, reviewer) appends structured events — the task it was given, every assistant message, every tool call and result, every interrupt, and the turn's outcome — to one append-only JSONL file per Linear issue, so a human debugging a run can read exactly what the agent saw and did without re-deriving it from stderr narration or Linear comments.

**Architecture:** A new leaf module, `clipse_agent.transcript`, owns the JSONL sink (`TranscriptWriter`) and is otherwise dependency-free — `dac.py` never imports it. `dac.drive_turn` gains an optional `event_sink: Callable[[dict], None] | None` parameter and is the single tap point: it already iterates every `messages`/`updates` stream chunk, so it is where `turn_start`/`assistant`/`tool_call`/`tool_result`/`interrupt`/`turn_end` events actually get produced, keyed off the real chunk shapes LangGraph yields (verified below, not guessed). The graph layer (`graphs/coder.py`, `graphs/reviewer.py`) threads a `TranscriptWriter | None` into `build_coder_graph`/`build_reviewer_graph` exactly the way `checkpointer` and `profile` already thread today — a construction-time kwarg, closure-captured into `make_run_dac`/`make_run_docs`, which binds it into `dac`'s `event_sink` with the lane/run_id/thread_id/assistant_id/model context it already has at node-execution time (`TranscriptWriter.bind(**context) -> event_sink`). `worker.py` gains a `--transcript` flag (default `""` = disabled) that builds the writer once and passes it as an `extra_kwargs["transcript"]`, mirroring exactly how `--model`/`--shell-allow-list` already become profile kwargs. On the Go side, `WorkerSpec.TranscriptPath` + `internal/spawn`'s `--transcript=` argv flag mirror `CheckpointDB`/`--checkpoint-db=` byte-for-byte; `dispatcher.transcriptPath` derives `<board_dir>/logs/<ISSUE>.transcript.jsonl` — next to the existing per-issue stderr log — the same way `checkpointDBPath` derives the checkpoint DB path. One file accumulates across every turn/lane/rework a given issue ever runs, because `dispatcher.transcriptPath` keys purely off the issue identifier, not the run or lane.

**Tech Stack:** Python stdlib only for the writer (`json`, `pathlib`, `time`, `sys` — no new dependency); existing LangChain/LangGraph message types (`AIMessage`, `AIMessageChunk`, `ToolMessage`) for the tap; Go stdlib (`path/filepath`) for the kernel side, mirroring `checkpointDBPath`'s existing pattern exactly.

## Global Constraints

- Python `>=3.13`, uv-managed (`cd agent && uv run ...`). No new runtime deps — this is a pure-stdlib module.
- TDD: a failing test first, then the minimal implementation, for every behavior change (both languages).
- `make test` (`test-go` + `test-py`) is the gate and must stay green after every task; `make lint` (`go vet` + `gofmt` + `ruff`) must stay clean.
- A transcript write failure must NEVER fail, block, or even perturb a run — `TranscriptWriter.emit` catches everything and logs to stderr. This is the one correctness invariant this whole feature is not allowed to violate.
- Go kernel stays LLM-free and stays exactly analogous to the existing `CheckpointDB`/`checkpointDBPath` wiring — no new vocabulary, no new decision points, just a second derived path next to it.
- Work on branch `feat/transcripts`. Commits: Conventional Commits, casual/lowercase, no trailing period, one concern each. Never `git add -A`/`git add .` at the repo root (per AGENTS.md, an untracked `.superpowers/` SDD ledger must never be committed).
- Signatures quoted below were copied from (or directly verified against) the real source as it stands at `main@f801d46` — `agent/src/clipse_agent/dac.py`, `graphs/coder.py`, `graphs/reviewer.py`, `worker.py`, `internal/spawn/{spawn,local}.go`, `dispatcher/spawn.go` — not guessed.

## File Structure

```
agent/
  src/clipse_agent/
    transcript.py                # new — TranscriptWriter
    dac.py                       # modify — event_sink tap in drive_turn
    worker.py                    # modify — --transcript flag + wiring
    graphs/
      coder.py                   # modify — transcript kwarg threading
      reviewer.py                # modify — transcript kwarg threading
  tests/
    test_transcript.py           # new
    test_dac.py                  # modify
    test_coder_graph.py          # modify
    test_reviewer_graph.py       # modify
    test_worker.py               # modify
  evals/
    harness.py                   # modify — run_coder_turn(transcript_path=...)
    test_coder_evals.py          # modify — C1 transcript assertion
internal/
  spawn/
    spawn.go                     # modify — WorkerSpec.TranscriptPath
    local.go                     # modify — --transcript= argv
    argv_test.go                 # modify
dispatcher/
  spawn.go                       # modify — transcriptPath() + wiring
  transcript_test.go             # new
AGENTS.md                        # modify — one bullet
```

---

### Task 0: Branch

- [ ] **Step 1: Create the working branch**

```bash
git checkout -b feat/transcripts
```

---

### Task 1: `clipse_agent.transcript` — the JSONL writer

**Files:**
- Create: `agent/src/clipse_agent/transcript.py`
- Test: `agent/tests/test_transcript.py`

**Interfaces:**
- Produces: `TranscriptWriter(path: str | Path)`; `.emit(event: Mapping[str, Any]) -> None` (append one JSON line, never raises); `.bind(**context: Any) -> Callable[[Mapping[str, Any]], None]` (returns a sink that merges `context` into every event before writing it — this is the callable `dac.drive_turn`'s `event_sink` parameter expects); `.path -> Path` (read-only, so callers/tests can confirm what a writer targets without reaching into a private attribute).

- [ ] **Step 1: Write the failing tests**

Create `agent/tests/test_transcript.py`:

```python
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
```

- [ ] **Step 2: Run to verify failure**

Run: `cd agent && uv run pytest tests/test_transcript.py -v`
Expected: FAIL / ERROR — `ModuleNotFoundError: No module named 'clipse_agent.transcript'` on every test.

- [ ] **Step 3: Implement**

Create `agent/src/clipse_agent/transcript.py`:

```python
"""Per-ticket agent transcript logging.

`TranscriptWriter` is an append-only JSONL sink: one JSON object per line,
one line per event. Multiple turns/lanes/reworks against the SAME issue all
append to the SAME file across the issue's whole lifetime (see
`graphs/coder.py`'s and `graphs/reviewer.py`'s `make_run_dac`, and
`dispatcher.transcriptPath` on the Go side, which keys the path purely off
the issue identifier).

A transcript is a debug/observability aid, never load-bearing for a run's
outcome: every write is wrapped in a catch-all that logs to stderr and
swallows the error, so a full disk, a permissions problem, or any other
write failure can never fail -- or even perturb -- the graph turn it was
trying to record.
"""

from __future__ import annotations

import json
import sys
import time
from collections.abc import Callable, Mapping
from pathlib import Path
from typing import Any


class TranscriptWriter:
    """Appends one JSON object per line to `path`.

    `path`'s parent directory is created lazily, on first write (not at
    construction) -- so building a `TranscriptWriter` for a path whose
    directory doesn't exist yet (e.g. a worker constructing one from a
    `--transcript` flag before anything else has touched the logs dir)
    never raises until the first real write, and that write itself never
    raises either (see `emit`).
    """

    def __init__(self, path: str | Path) -> None:
        self._path = Path(path)

    @property
    def path(self) -> Path:
        return self._path

    def emit(self, event: Mapping[str, Any]) -> None:
        """Append one event as a single JSON line.

        `ts` (wall-clock seconds) is stamped here, at write time, unioned
        over anything already in `event` -- every event's timestamp
        reflects when it was actually written, not when some caller may
        have queued it.

        Never raises: a write failure is logged to stderr and swallowed.
        `default=str` guards any non-JSON-native value a caller passes in
        an event's fields (e.g. a raw exception object, an SDK type's own
        repr) from turning a *logging* failure into a crash.
        """
        row = {**event, "ts": time.time()}
        try:
            self._path.parent.mkdir(parents=True, exist_ok=True)
            with self._path.open("a") as f:
                f.write(json.dumps(row, default=str) + "\n")
        except Exception as exc:  # noqa: BLE001 -- see class docstring: never raise into a run
            sys.stderr.write(f"transcript: failed to write event {row.get('event')!r}: {exc}\n")

    def bind(self, **context: Any) -> Callable[[Mapping[str, Any]], None]:
        """Return a sink function that merges `context` into every event
        before writing it.

        `context` is whatever the caller already knows when it builds the
        sink -- lane, run_id, thread_id, assistant_id, model -- so the
        per-event call sites in `dac.drive_turn` never have to know or
        re-derive any of it. The returned callable is exactly what
        `dac.drive_turn` treats as an opaque `event_sink`: one event dict
        in, nothing out. Context keys are merged BEFORE the event's own
        keys, so an event can still override a context field for itself if
        a future event type ever needs to (none does today).
        """

        def _sink(event: Mapping[str, Any]) -> None:
            self.emit({**context, **event})

        return _sink
```

- [ ] **Step 4: Run tests green**

Run: `cd agent && uv run pytest tests/test_transcript.py -v`
Expected: 7 PASS.

- [ ] **Step 5: Lint + commit**

```bash
cd agent && uvx ruff check src/clipse_agent/transcript.py tests/test_transcript.py && cd ..
git add agent/src/clipse_agent/transcript.py agent/tests/test_transcript.py
git commit -m "feat(agent): add TranscriptWriter, an append-only jsonl event sink"
```

---

### Task 2: `dac.drive_turn` — the event_sink tap

The tap point has to match the REAL chunk shapes `agent_graph.astream(stream_mode=["messages", "updates"], subgraphs=True)` yields, verified directly rather than assumed:

- A `messages`-mode chunk is `(message_obj, metadata)`. During real token streaming, `message_obj` is an `AIMessageChunk` (LangGraph's `on_llm_new_token` callback forwards each raw provider delta's `ChatGenerationChunk.message` — one UNMERGED delta per chunk, `langgraph/pregel/_messages.py`); a complete, non-streamed `AIMessage`/`ToolMessage` can also appear (e.g. a subgraph's finished output folded into state).
- A single `AIMessageChunk`'s own `.tool_calls` is often incomplete mid-stream (a tool call's `args` JSON arrives fragmented across many chunks) — verified directly:
  ```python
  >>> from langchain_core.messages import AIMessageChunk
  >>> c1 = AIMessageChunk(content="", tool_call_chunks=[{"name": "shell", "args": '{"cmd":', "id": "call_1", "index": 0}])
  >>> c2 = AIMessageChunk(content="", tool_call_chunks=[{"name": None, "args": ' "ls"}', "id": None, "index": 0}])
  >>> c1.tool_calls        # alone: incomplete
  [{'name': 'shell', 'args': {}, 'id': 'call_1', 'type': 'tool_call'}]
  >>> (c1 + c2).tool_calls  # merged via AIMessageChunk.__add__ (LangChain's own documented reassembly): complete
  [{'name': 'shell', 'args': {'cmd': 'ls'}, 'id': 'call_1', 'type': 'tool_call'}]
  ```
  So a `tool_call` event must be built from the chunks summed via `+` (LangChain's own mechanism for this), never from one chunk's `.tool_calls` alone.
- A `ToolMessage` chunk is currently skipped entirely by `_accumulate_message_chunk` (`not isinstance(message_obj, AIMessage)` returns `(0, 0)`) — it has no `usage_metadata` and is never chunked (a tool's result lands as one complete message, never streamed in fragments), so it needs no accumulation, only a direct one-shot emit.
- An `updates`-mode chunk carrying `__interrupt__` is already detected by `_extract_interrupt_payload`; nothing else in `updates` mode is consumed.

Given that, a "logical message" (all chunks sharing one `.id`) is flushed as at most one `assistant` event (its accumulated text) and zero or more `tool_call` events (from its merged chunk-sum), either when a chunk for a DIFFERENT message id arrives, or once more after the stream ends (for whichever message was still pending when the loop exited — including a token-ceiling abort mid-message, which is still useful to see in the transcript).

**Files:**
- Modify: `agent/src/clipse_agent/dac.py`
- Test: `agent/tests/test_dac.py`

**Interfaces:**
- Produces: `dac.EventSink = Callable[[dict[str, Any]], None]`; `drive_turn(..., event_sink: EventSink | None = None)`. Event shapes: `{"event": "turn_start", "task_text": str | None}` (emitted once, before streaming starts — `None` on a `resume` call, since there is no fresh `task_text`); `{"event": "assistant", "text": str}`; `{"event": "tool_call", "name": str | None, "args": dict}`; `{"event": "tool_result", "name": str | None, "status": str, "content": str}` (content truncated to 8,000 chars); `{"event": "interrupt", "payload": str}` (a `repr()` of the interrupt payload list); `{"event": "turn_end", "outcome_hint": str, "tokens_in": int, "tokens_out": int}` (emitted once, only on a clean return from the streaming loop — an exception already surfaces as `DacError`, out of scope for this module's own turn bookkeeping). `event_sink` is assumed not to raise (its real caller, `TranscriptWriter.bind`'s returned sink, already swallows everything); `drive_turn` does not wrap sink calls itself.

- [ ] **Step 1: Write the failing tests**

Append to `agent/tests/test_dac.py` (add `AIMessageChunk` to the existing `from langchain_core.messages import AIMessage, ToolMessage` import at the top of the file, making it `from langchain_core.messages import AIMessage, AIMessageChunk, ToolMessage`):

```python
# ---------------------------------------------------------------------------
# drive_turn -- event_sink (transcript tap)
# ---------------------------------------------------------------------------


def _ai_chunk(
    *,
    text: str = "",
    tokens_in: int = 0,
    tokens_out: int = 0,
    message_id: str | None = None,
    tool_call_chunks: list[dict[str, Any]] | None = None,
) -> AIMessageChunk:
    usage = (
        {"input_tokens": tokens_in, "output_tokens": tokens_out, "total_tokens": tokens_in + tokens_out}
        if tokens_in or tokens_out
        else None
    )
    return AIMessageChunk(
        id=message_id,
        content=[{"type": "text", "text": text}] if text else "",
        usage_metadata=usage,
        tool_call_chunks=tool_call_chunks or [],
    )


def test_drive_turn_emits_turn_start_before_streaming_and_turn_end_after():
    graph = _FakeAgentGraph(
        [((), "messages", (_ai_message("done", tokens_in=5, tokens_out=2), {}))]
    )
    events: list[dict[str, Any]] = []

    asyncio.run(
        dac.drive_turn(graph, _CONFIG, task_text="fix it", max_tokens=None, event_sink=events.append)
    )

    assert events[0] == {"event": "turn_start", "task_text": "fix it"}
    assert events[-1] == {
        "event": "turn_end",
        "outcome_hint": "completed",
        "tokens_in": 5,
        "tokens_out": 2,
    }


def test_drive_turn_turn_start_carries_none_task_text_on_resume():
    graph = _FakeAgentGraph([])
    events: list[dict[str, Any]] = []

    asyncio.run(
        dac.drive_turn(graph, _CONFIG, resume={"int-1": {}}, max_tokens=None, event_sink=events.append)
    )

    assert events[0] == {"event": "turn_start", "task_text": None}


def test_drive_turn_never_calls_event_sink_when_not_given():
    # Every other test in this file omits event_sink and must be unaffected
    # -- this is the explicit regression guard for that default.
    graph = _FakeAgentGraph(
        [((), "messages", (_ai_message("done", tokens_in=1, tokens_out=1), {}))]
    )

    result = asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None))

    assert result.outcome_hint == "completed"  # ran to completion with no sink at all


def test_drive_turn_flushes_one_assistant_event_per_logical_message():
    graph = _FakeAgentGraph(
        [
            ((), "messages", (_ai_message("Reading the ", tokens_in=1, tokens_out=1, message_id="m1"), {})),
            ((), "messages", (_ai_message("ticket now.", tokens_in=1, tokens_out=1, message_id="m1"), {})),
            ((), "messages", (_ai_message("STATUS: done", tokens_in=1, tokens_out=1, message_id="m2"), {})),
        ]
    )
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None, event_sink=events.append))

    assistant_events = [e for e in events if e["event"] == "assistant"]
    assert assistant_events == [
        {"event": "assistant", "text": "Reading the ticket now."},
        {"event": "assistant", "text": "STATUS: done"},
    ]


def test_drive_turn_emits_tool_call_with_args_merged_across_streamed_chunks():
    # A tool call's `args` JSON streams across several chunks -- only the
    # merged (AIMessageChunk.__add__) result has the complete, parseable
    # args; a naive read of any single chunk's own .tool_calls would show
    # `args: {}` (see this task's own docstring-level verification above).
    graph = _FakeAgentGraph(
        [
            (
                (),
                "messages",
                (_ai_chunk(message_id="m1", tool_call_chunks=[{"name": "shell", "args": '{"cmd":', "id": "call-1", "index": 0}]), {}),
            ),
            (
                (),
                "messages",
                (_ai_chunk(message_id="m1", tool_call_chunks=[{"name": None, "args": ' "ls"}', "id": None, "index": 0}]), {}),
            ),
            ((), "messages", (_ai_message("next message", tokens_in=1, tokens_out=1, message_id="m2"), {})),
        ]
    )
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None, event_sink=events.append))

    tool_calls = [e for e in events if e["event"] == "tool_call"]
    assert tool_calls == [{"event": "tool_call", "name": "shell", "args": {"cmd": "ls"}}]


def test_drive_turn_emits_tool_result_for_a_tool_message():
    graph = _FakeAgentGraph(
        [((), "messages", (ToolMessage(content="ok", name="shell", tool_call_id="call-1", status="success"), {}))]
    )
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None, event_sink=events.append))

    tool_results = [e for e in events if e["event"] == "tool_result"]
    assert tool_results == [{"event": "tool_result", "name": "shell", "status": "success", "content": "ok"}]


def test_drive_turn_truncates_oversized_tool_result_content():
    huge = "x" * 9_000
    graph = _FakeAgentGraph([((), "messages", (ToolMessage(content=huge, name="cat", tool_call_id="call-1"), {}))])
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="t", max_tokens=None, event_sink=events.append))

    tool_result = next(e for e in events if e["event"] == "tool_result")
    assert len(tool_result["content"]) < 9_000
    assert tool_result["content"].startswith("x" * 100)


def test_drive_turn_emits_interrupt_event_with_payload_repr():
    action = {"action_requests": [{"name": "shell", "args": {"command": "rm -rf /"}}]}
    graph = _FakeAgentGraph([((), "updates", {"__interrupt__": [_interrupt(action)]})])
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=None, event_sink=events.append))

    interrupt_events = [e for e in events if e["event"] == "interrupt"]
    assert interrupt_events == [{"event": "interrupt", "payload": repr([action])}]


def test_drive_turn_flushes_pending_message_at_ceiling_abort():
    # A token-ceiling abort breaks the loop mid-message (no id transition to
    # trigger a flush) -- the post-loop flush must still catch it.
    graph = _FakeAgentGraph(
        [((), "messages", (_ai_message("partial narration", tokens_in=150, tokens_out=1, message_id="m1"), {}))]
    )
    events: list[dict[str, Any]] = []

    asyncio.run(dac.drive_turn(graph, _CONFIG, task_text="go", max_tokens=100, event_sink=events.append))

    assistant_events = [e for e in events if e["event"] == "assistant"]
    assert assistant_events == [{"event": "assistant", "text": "partial narration"}]
    assert events[-1]["event"] == "turn_end"
    assert events[-1]["outcome_hint"] == "interrupted"
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && uv run pytest tests/test_dac.py -k "event_sink or turn_start or turn_end or tool_call or tool_result or interrupt_event or ceiling_abort" -v`
Expected: every new test FAILS — `TypeError: drive_turn() got an unexpected keyword argument 'event_sink'`.

- [ ] **Step 3: Implement**

In `agent/src/clipse_agent/dac.py`:

1. Update the message import (currently `from langchain_core.messages import AIMessage`):

```python
from langchain_core.messages import AIMessage, AIMessageChunk, ToolMessage
```

2. Add `from collections.abc import Callable` to the imports (currently only `from dataclasses import dataclass` and `from typing import TYPE_CHECKING, Any, Literal` — add the new import as its own line, alphabetically before `dataclasses`).

3. Add the type alias and truncation helper right after `OutcomeHint = Literal["completed", "interrupted"]`:

```python
# One event dict in, nothing out. The real caller is
# `clipse_agent.transcript.TranscriptWriter.bind(...)`'s returned sink, which
# already merges lane/run_id/thread_id/assistant_id/model context into every
# event and never raises -- drive_turn does not wrap event_sink calls itself.
EventSink = Callable[[dict[str, Any]], None]

# `tool_result` events truncate a ToolMessage's content to this many chars --
# generous enough to keep a shell command's real output/error legible in the
# transcript, small enough that one runaway tool result can't balloon the
# per-issue transcript file.
_TOOL_RESULT_CONTENT_LIMIT = 8_000


def _truncate_for_transcript(text: str, limit: int = _TOOL_RESULT_CONTENT_LIMIT) -> str:
    if len(text) <= limit:
        return text
    return text[:limit] + f"...<truncated at {limit} chars>"
```

4. Add `_flush_pending` right before `_accumulate_message_chunk`:

```python
def _flush_pending(event_sink: EventSink | None, pending: dict[str, Any]) -> None:
    """Emit the just-finished logical AIMessage's accumulated text and tool
    calls as transcript events (`assistant`, then one `tool_call` per call).

    A logical message is "finished" once a chunk for a DIFFERENT message id
    arrives (see `_accumulate_message_chunk`) or the stream ends -- `drive_turn`
    calls this once more after its loop to flush whatever message was still
    pending when the loop exited (including a token-ceiling abort mid-message).
    A no-op when `event_sink` is None (transcripts disabled) or `pending` has
    never seen a chunk yet (the very first call, before any message arrived).
    """
    if event_sink is None or "parts" not in pending:
        return
    text = "".join(pending.get("parts") or [])
    if text:
        event_sink({"event": "assistant", "text": text})
    for call in pending.get("tool_calls") or []:
        event_sink({"event": "tool_call", "name": call.get("name"), "args": call.get("args")})
```

5. Rewrite `_accumulate_message_chunk`:

```python
def _accumulate_message_chunk(
    data: tuple[Any, dict[str, Any]],
    text_parts: list[str],
    last_message: dict[str, Any],
    event_sink: EventSink | None = None,
) -> tuple[int, int]:
    """Fold one `messages`-mode chunk's usage/text/tool-calls into the
    running turn, and (when `event_sink` is set) emit transcript events.

    Mutates `text_parts` in place with any text blocks found (the whole
    turn's text), and `last_message` (its `"id"`/`"parts"`/`"tool_calls"`/
    `"chunk_sum"` keys) so that only the FINAL AIMessage's data survives:
    streaming delivers one logical message as several chunks sharing an id,
    so `parts` accumulates text across those, and `chunk_sum` accumulates the
    raw chunks themselves via `AIMessageChunk.__add__` -- LangChain's own
    documented way to reassemble a streamed tool call's fragmented `args`
    JSON (a single chunk's own `.tool_calls` is often incomplete mid-stream;
    only the merged sum is reliably complete). All four keys reset whenever a
    chunk carrying a new message id arrives. `tool_calls` always holds the
    best snapshot for the CURRENT message: `chunk_sum.tool_calls` for a
    genuine `AIMessageChunk`, or the message's own `.tool_calls` for a
    complete, non-chunk `AIMessage` (some `messages`-mode emissions -- e.g. a
    subgraph's finished output folded into state -- are already complete,
    not chunks; merging those with `+` would raise).

    On a message-id transition, the JUST-FINISHED message's accumulated text
    and tool calls are flushed as transcript events (`_flush_pending`) BEFORE
    the reset; `drive_turn` calls `_flush_pending` once more after the stream
    ends, for whichever message was still pending when the loop exited.

    Returns the `(input_tokens, output_tokens)` this chunk contributes;
    always `(0, 0)` for anything that is not an `AIMessage` (e.g. a
    `ToolMessage`, which has no `usage_metadata` and whose `content_blocks`
    are tool output, not assistant text -- and which never touches
    `last_message`, so an interleaved tool result can't reset the final
    message's accumulated text). When `event_sink` is set, a `ToolMessage`
    instead emits its own `tool_result` event directly, with no accumulation
    needed: a tool's result arrives as one complete message, never streamed
    in fragments.
    """
    message_obj, _metadata = data
    if not isinstance(message_obj, AIMessage):
        if event_sink is not None and isinstance(message_obj, ToolMessage):
            event_sink(
                {
                    "event": "tool_result",
                    "name": message_obj.name,
                    "status": message_obj.status,
                    "content": _truncate_for_transcript(str(message_obj.content)),
                }
            )
        return 0, 0

    usage = getattr(message_obj, "usage_metadata", None) or {}
    tokens_in = usage.get("input_tokens", 0) or 0
    tokens_out = usage.get("output_tokens", 0) or 0

    message_id = getattr(message_obj, "id", None)
    if "parts" not in last_message or message_id != last_message.get("id"):
        _flush_pending(event_sink, last_message)
        last_message.clear()
        last_message["id"] = message_id
        last_message["parts"] = []

    for block in getattr(message_obj, "content_blocks", None) or ():
        if isinstance(block, dict) and block.get("type") == "text":
            text = block.get("text", "")
            if text:
                text_parts.append(text)
                last_message["parts"].append(text)

    if isinstance(message_obj, AIMessageChunk):
        prior_sum = last_message.get("chunk_sum")
        merged = message_obj if prior_sum is None else prior_sum + message_obj
        last_message["chunk_sum"] = merged
        last_message["tool_calls"] = merged.tool_calls
    elif message_obj.tool_calls:
        last_message["tool_calls"] = message_obj.tool_calls

    return tokens_in, tokens_out
```

6. Update `drive_turn`'s signature and body. Signature:

```python
async def drive_turn(
    agent_graph: Pregel[Any, Any, Any, Any],
    config: dict[str, Any],
    *,
    task_text: str | None = None,
    resume: Any | None = None,
    max_tokens: int | None,
    event_sink: EventSink | None = None,
) -> DacTurnResult:
```

Append to its existing docstring (right before the `Raises:` section):

```
    `event_sink`, when given, receives one dict per transcript event as the
    turn is driven: `turn_start` (with `task_text`, `None` on a `resume`
    call) before the stream starts; `assistant`/`tool_call` once per logical
    message (flushed on a message-id transition or at stream end -- see
    `_flush_pending`); `tool_result` per `ToolMessage`; `interrupt` when one
    is detected; and `turn_end` (`outcome_hint`, `tokens_in`, `tokens_out`)
    once the stream ends normally. `turn_end` is emitted only on a clean
    return from the loop below, not from the `except` branch -- an exception
    here already surfaces as a `DacError` the caller maps to a blocked run,
    which is out of scope for this module's own turn bookkeeping.
```

Body (replacing everything from the `if (task_text is None) == (resume is None):` guard through the final `return`):

```python
    if (task_text is None) == (resume is None):
        raise ValueError(
            "drive_turn requires exactly one of task_text (fresh turn) or "
            "resume (continuation after an interrupt), not both or neither"
        )

    if event_sink is not None:
        event_sink({"event": "turn_start", "task_text": task_text})

    stream_input: dict[str, Any] | Command = (
        _fresh_turn_input(task_text) if task_text is not None else Command(resume=resume)
    )

    tokens_in = 0
    tokens_out = 0
    text_parts: list[str] = []
    last_message: dict[str, Any] = {}
    interrupt_payload: list[Any] | None = None
    token_ceiling_exceeded = False

    try:
        async for _namespace, mode, data in agent_graph.astream(
            stream_input,
            stream_mode=["messages", "updates"],
            subgraphs=True,
            config=config,
        ):
            if mode == "updates" and isinstance(data, dict) and "__interrupt__" in data:
                payload = _extract_interrupt_payload(data)
                if payload is not None:
                    interrupt_payload = payload
                    if event_sink is not None:
                        event_sink({"event": "interrupt", "payload": repr(payload)})
            elif mode == "messages":
                turn_in, turn_out = _accumulate_message_chunk(data, text_parts, last_message, event_sink)
                tokens_in += turn_in
                tokens_out += turn_out

                if max_tokens is not None and turn_in > max_tokens:
                    token_ceiling_exceeded = True
                    break
    except Exception as exc:
        thread_id = config.get("configurable", {}).get("thread_id")
        raise DacError(
            f"DAC turn failed while streaming the agent graph "
            f"(thread_id={thread_id!r}): {exc}"
        ) from exc

    _flush_pending(event_sink, last_message)

    outcome_hint: OutcomeHint = (
        "interrupted" if interrupt_payload is not None or token_ceiling_exceeded else "completed"
    )

    if event_sink is not None:
        event_sink(
            {
                "event": "turn_end",
                "outcome_hint": outcome_hint,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
            }
        )

    return DacTurnResult(
        outcome_hint=outcome_hint,
        final_text="".join(text_parts),
        tokens_in=tokens_in,
        tokens_out=tokens_out,
        interrupt_payload=interrupt_payload,
        token_ceiling_exceeded=token_ceiling_exceeded,
        last_text="".join(last_message.get("parts", [])),
    )
```

- [ ] **Step 4: Run the full dac test file**

Run: `cd agent && uv run pytest tests/test_dac.py -v`
Expected: ALL PASS (44 existing + 9 new = 53).

- [ ] **Step 5: Run the full Python suite + lint**

Run: `cd agent && uv run pytest && uvx ruff check .`
Expected: 268 pass (259 baseline + 9), no lint errors.

- [ ] **Step 6: Commit**

```bash
git add agent/src/clipse_agent/dac.py agent/tests/test_dac.py
git commit -m "feat(agent): tap drive_turn's stream loop with an optional transcript event_sink"
```

---

### Task 3: Thread `transcript` through the Coder and Reviewer graphs

`checkpointer` and `profile` already thread the SAME way: a `build_coder_graph(...)`/`build_reviewer_graph(...)` keyword argument, closure-captured into `make_run_dac`/`make_run_docs`, never carried on `CoderState`/`ReviewerState`. `transcript` follows that exact precedent — it is construction-time infrastructure, not turn data, so it belongs in the closure, not the state dict. What's genuinely per-invocation (`run_id`, `thread_id`) still has to come from `state` inside the node body at call time, exactly like `_dac_config(state["thread_id"])` already does today — so the bound sink itself is built inside each node's `_node` function, not in `make_run_dac`'s outer factory scope.

**Files:**
- Modify: `agent/src/clipse_agent/graphs/coder.py` (`make_run_dac`, `make_run_docs`, `build_coder_graph`)
- Modify: `agent/src/clipse_agent/graphs/reviewer.py` (`make_run_dac`, `build_reviewer_graph`)
- Test: `agent/tests/test_coder_graph.py`, `agent/tests/test_reviewer_graph.py`

**Interfaces:**
- Produces: `make_run_dac(profile, agent_factory, turn_driver, checkpointer, transcript: TranscriptWriter | None = None)` (both modules); `make_run_docs(profile, agent_factory, turn_driver, checkpointer, transcript: TranscriptWriter | None = None)` (coder only); `build_coder_graph(..., transcript: TranscriptWriter | None = None)`; `build_reviewer_graph(..., transcript: TranscriptWriter | None = None)`. The coding turn binds `lane="coder"`, the docs turn `lane="coder_docs"`, the reviewer turn `lane="reviewer"` — all three also bind `run_id`, `thread_id` (from `state`), `assistant_id`, `model` (from `profile`).

- [ ] **Step 1: Write the failing tests**

Append to `agent/tests/test_coder_graph.py`. First add `from clipse_agent.profiles.coder import get_coder_profile` to the imports, and this fake near the file's other fakes (right after `_fake_turn_driver_by_turn`):

```python
class _FakeTranscript:
    """Stand-in for `transcript.TranscriptWriter`: records every `bind()`
    call's context, and merges that context into every event a bound sink
    receives -- without ever touching a real file."""

    def __init__(self) -> None:
        self.bind_calls: list[dict[str, Any]] = []
        self.events: list[dict[str, Any]] = []

    def bind(self, **context: Any) -> Callable[[dict[str, Any]], None]:
        self.bind_calls.append(context)

        def _sink(event: dict[str, Any]) -> None:
            self.events.append({**context, **event})

        return _sink
```

Then, near the other `make_run_dac`-focused tests (after `test_run_dac_sends_normal_task_text_when_no_merge_conflict_files`):

```python
def test_run_dac_threads_transcript_event_sink_to_turn_driver(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)
    profile = get_coder_profile()
    transcript = _FakeTranscript()
    node = coder.make_run_dac(
        profile, _fake_agent_factory([]), _fake_turn_driver(turn_result, turn_calls), None, transcript
    )
    state: coder.CoderState = {
        "cwd": str(tmp_path),
        "run_id": "run-9",
        "thread_id": "thread-9",
        "task_text": "Build the widget factory.",
    }

    asyncio.run(node(state))

    assert transcript.bind_calls == [
        {
            "lane": "coder",
            "run_id": "run-9",
            "thread_id": "thread-9",
            "assistant_id": profile.assistant_id,
            "model": profile.model,
        }
    ]
    assert callable(turn_calls[0]["event_sink"])


def test_run_dac_passes_no_event_sink_when_transcript_is_none(tmp_path):
    turn_calls: list[dict[str, Any]] = []
    turn_result = DacTurnResult(outcome_hint="completed", final_text="done", tokens_in=1, tokens_out=1)
    node = coder.make_run_dac(
        "unused-profile", _fake_agent_factory([]), _fake_turn_driver(turn_result, turn_calls), None
    )
    state: coder.CoderState = {"cwd": str(tmp_path), "thread_id": "t", "task_text": "x"}

    asyncio.run(node(state))

    assert turn_calls[0]["event_sink"] is None
```

And near the full-graph happy-path test (after `test_happy_path_runs_full_node_order_and_emits_needs_review`):

```python
def test_build_coder_graph_threads_transcript_to_both_dac_turns(tmp_path):
    runner = _base_runner()
    turn_calls: list[dict[str, Any]] = []
    code_result = DacTurnResult(outcome_hint="completed", final_text="code done", tokens_in=1, tokens_out=1)
    docs_result = DacTurnResult(outcome_hint="completed", final_text="docs done", tokens_in=1, tokens_out=1)
    transcript = _FakeTranscript()

    graph = coder.build_coder_graph(
        agent_factory=_fake_agent_factory([]),
        turn_driver=_fake_turn_driver_by_turn(code_result, docs_result, turn_calls),
        run_command=runner,
        transcript=transcript,
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

    asyncio.run(_drive(graph, input_state, config))

    lanes = {call.get("lane") for call in transcript.bind_calls}
    assert lanes == {"coder", "coder_docs"}
    for call in turn_calls:
        assert callable(call["event_sink"])
```

Append to `agent/tests/test_reviewer_graph.py` — add the same `_FakeTranscript` fake near its own `_fake_agent_factory`/`_fake_turn_driver`, then after `test_run_dac_stashes_last_text`:

```python
def test_run_dac_threads_transcript_event_sink_to_turn_driver() -> None:
    calls: list[dict[str, Any]] = []
    turn = DacTurnResult(outcome_hint="completed", final_text="ok", tokens_in=1, tokens_out=1)
    profile = get_reviewer_profile()
    transcript = _FakeTranscript()
    node = reviewer.make_run_dac(
        profile, _fake_agent_factory(calls), _fake_turn_driver(turn, calls), None, transcript
    )

    asyncio.run(node({"run_id": "run-3", "thread_id": "thread-3", "cwd": "/tmp", "task_text": "review"}))

    assert transcript.bind_calls == [
        {
            "lane": "reviewer",
            "run_id": "run-3",
            "thread_id": "thread-3",
            "assistant_id": profile.assistant_id,
            "model": profile.model,
        }
    ]
    assert callable(calls[0]["event_sink"])


def test_run_dac_passes_no_event_sink_when_transcript_is_none() -> None:
    calls: list[dict[str, Any]] = []
    turn = DacTurnResult(outcome_hint="completed", final_text="ok", tokens_in=1, tokens_out=1)
    node = reviewer.make_run_dac(
        get_reviewer_profile(), _fake_agent_factory(calls), _fake_turn_driver(turn, calls), None
    )

    asyncio.run(node({"thread_id": "t", "cwd": "/tmp", "task_text": "review"}))

    assert calls[0]["event_sink"] is None
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && uv run pytest tests/test_coder_graph.py tests/test_reviewer_graph.py -k transcript -v`
Expected: every new test FAILS — `TypeError: make_run_dac() takes from 4 to 4 positional arguments but 5 were given` (or `build_coder_graph() got an unexpected keyword argument 'transcript'`).

- [ ] **Step 3: Implement**

In `agent/src/clipse_agent/graphs/coder.py`:

1. Add the import: `from clipse_agent.transcript import TranscriptWriter`.

2. `make_run_dac`'s signature and node body:

```python
def make_run_dac(
    profile: CoderProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
    transcript: TranscriptWriter | None = None,
) -> Callable[[CoderState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh task turn normally (the normal
    issue/rework task, or a conflict-resolution task when `sync_base` left a
    merge in progress -- see `_coding_task_text`), or a `resume` of a
    previously-interrupted turn when `resume_payload` is set.

    `transcript`, when given, is bound into this turn's `event_sink` fresh on
    every call -- `run_id`/`thread_id` are only known once `state` arrives at
    invocation time, unlike `profile`/`checkpointer`, which are already fixed
    when this factory runs (see the module's own build_coder_graph docstring).
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
        config = _dac_config(state["thread_id"])
        max_tokens = state.get("max_tokens")
        resume_payload = state.get("resume_payload")
        event_sink = (
            transcript.bind(
                lane="coder",
                run_id=state["run_id"],
                thread_id=state["thread_id"],
                assistant_id=profile.assistant_id,
                model=profile.model,
            )
            if transcript is not None
            else None
        )

        if resume_payload is not None:
            turn_result = await turn_driver(
                agent_graph, config, resume=resume_payload, max_tokens=max_tokens, event_sink=event_sink
            )
        else:
            turn_result = await turn_driver(
                agent_graph,
                config,
                task_text=_coding_task_text(state),
                max_tokens=max_tokens,
                event_sink=event_sink,
            )

        # Parse the STATUS/TITLE/HANDOFF tail once here and stash blocked_reason
        # so both route_after_dac (blocked -> emit_result) and emit_result (the
        # blocked summary) read a single derived value. Always written -- "" on a
        # non-blocked turn -- so a checkpointed prior turn's reason can't linger.
        tail = parse_structured_tail(turn_result.last_text)
        return {
            "dac_outcome_hint": turn_result.outcome_hint,
            "dac_summary": turn_result.final_text,
            "dac_last_text": turn_result.last_text,
            "blocked_reason": tail.blocked_reason if tail.status == "blocked" else "",
            "tokens_in": turn_result.tokens_in,
            "tokens_out": turn_result.tokens_out,
            "interrupt_payload": turn_result.interrupt_payload,
            "token_ceiling_exceeded": turn_result.token_ceiling_exceeded,
        }

    return _node
```

3. `make_run_docs`'s signature and node body (same pattern, `lane="coder_docs"`):

```python
def make_run_docs(
    profile: CoderProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
    transcript: TranscriptWriter | None = None,
) -> Callable[[CoderState], Awaitable[dict[str, Any]]]:
    """Drive one best-effort documentation DAC turn in the coder's worktree.

    Runs only on the clean path (see route_after_dac), after the coding turn
    already produced a review-ready change. Any docs the agent writes sit next
    to the code edits and are folded into the same commit by `make_commit`'s
    `git add -A` -- same commit, same PR, no separate branch.

    Best-effort by construction: it returns ONLY the private `doc_*` keys and
    never `interrupt_payload`/`token_ceiling_exceeded`/`dac_summary`, so a
    doc-turn ceiling, interrupt, or outright exception can neither turn this
    run into `blocked` nor rename the PR -- the coding turn's review-ready PR
    ships regardless. A ceiling/interrupt on the *coding* turn, by contrast,
    still blocks (route_after_dac skips this node entirely), because there the
    change itself may be incomplete or need a human. Uses its own `::docs-dac`
    thread namespace so it never resumes the coding turn's message history.

    `transcript`, when given, is bound fresh per invocation with
    `lane="coder_docs"` -- same reasoning as `make_run_dac`'s own docstring.
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        event_sink = (
            transcript.bind(
                lane="coder_docs",
                run_id=state["run_id"],
                thread_id=state["thread_id"],
                assistant_id=profile.assistant_id,
                model=profile.model,
            )
            if transcript is not None
            else None
        )
        try:
            agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
            turn_result = await turn_driver(
                agent_graph,
                _docs_dac_config(state["thread_id"]),
                task_text=_docs_task_text(state),
                max_tokens=_docs_max_tokens(state),
                event_sink=event_sink,
            )
        except Exception as exc:  # noqa: BLE001 -- docs are non-critical; degrade, never block the PR
            return {"doc_summary": f"Documentation step skipped (error): {exc}", "doc_tokens_in": 0, "doc_tokens_out": 0}

        if turn_result.token_ceiling_exceeded:
            summary = "Documentation step skipped: token budget reached before it finished."
        elif turn_result.interrupt_payload is not None:
            summary = "Documentation step skipped: it needed input it could not resolve on its own."
        else:
            summary = turn_result.final_text

        return {
            "doc_summary": summary,
            "doc_tokens_in": turn_result.tokens_in,
            "doc_tokens_out": turn_result.tokens_out,
        }

    return _node
```

(Only the two things that changed from the existing body: the new `event_sink` block at the top of `_node`, and `event_sink=event_sink` added to the `turn_driver(...)` call — everything else is unchanged.)

4. `build_coder_graph`'s signature gains `transcript: TranscriptWriter | None = None` (add it after `run_command` in the keyword-only parameter list), and both node-registration calls thread it through:

```python
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer, transcript))
    graph.add_node("run_docs", make_run_docs(resolved_docs_profile, agent_factory, turn_driver, checkpointer, transcript))
```

Add one sentence to `build_coder_graph`'s docstring, after the existing paragraph about `agent_factory`/`turn_driver`/`run_command` defaults: `` `transcript` (default `None` = disabled) threads through to both DAC turns the same way `checkpointer` does: a construction-time value closed over by `make_run_dac`/`make_run_docs`, never carried on `CoderState`. ``

In `agent/src/clipse_agent/graphs/reviewer.py`:

1. Add the import: `from clipse_agent.transcript import TranscriptWriter`.

2. `make_run_dac`'s signature and node body:

```python
def make_run_dac(
    profile: ReviewerProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
    transcript: TranscriptWriter | None = None,
) -> Callable[[ReviewerState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh `task_text` turn normally, or a
    `resume` of a previously-interrupted turn when `resume_payload` is set.

    Structurally identical to `graphs.coder.make_run_dac` (including how it
    binds `transcript` fresh per invocation, since `run_id`/`thread_id` only
    arrive with `state`), but intentionally **not** imported from there: it
    must call this module's own `_dac_config` (a distinct thread-namespace
    suffix), not Coder's -- see that function's docstring for the cross-lane
    checkpoint collision this prevents. Everything else about driving a DAC
    turn is genuinely lane-agnostic, hence the otherwise-identical body.
    """

    async def _node(state: ReviewerState) -> dict[str, Any]:
        agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
        config = _dac_config(state["thread_id"])
        max_tokens = state.get("max_tokens")
        resume_payload = state.get("resume_payload")
        event_sink = (
            transcript.bind(
                lane="reviewer",
                run_id=state["run_id"],
                thread_id=state["thread_id"],
                assistant_id=profile.assistant_id,
                model=profile.model,
            )
            if transcript is not None
            else None
        )

        if resume_payload is not None:
            turn_result = await turn_driver(
                agent_graph, config, resume=resume_payload, max_tokens=max_tokens, event_sink=event_sink
            )
        else:
            turn_result = await turn_driver(
                agent_graph, config, task_text=state.get("task_text", ""), max_tokens=max_tokens, event_sink=event_sink
            )

        return {
            "dac_outcome_hint": turn_result.outcome_hint,
            "dac_summary": turn_result.final_text,
            "dac_last_text": turn_result.last_text,
            "tokens_in": turn_result.tokens_in,
            "tokens_out": turn_result.tokens_out,
            "interrupt_payload": turn_result.interrupt_payload,
            "token_ceiling_exceeded": turn_result.token_ceiling_exceeded,
        }

    return _node
```

3. `build_reviewer_graph`'s signature gains `transcript: TranscriptWriter | None = None` (after `run_command`), threaded into the one node-registration call:

```python
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer, transcript))
```

- [ ] **Step 4: Run the full graph test files**

Run: `cd agent && uv run pytest tests/test_coder_graph.py tests/test_reviewer_graph.py -v`
Expected: ALL PASS.

- [ ] **Step 5: Run the full Python suite + lint**

Run: `cd agent && uv run pytest && uvx ruff check .`
Expected: 273 pass (268 from Task 2 + 5 new), no lint errors.

- [ ] **Step 6: Commit**

```bash
git add agent/src/clipse_agent/graphs/coder.py agent/src/clipse_agent/graphs/reviewer.py agent/tests/test_coder_graph.py agent/tests/test_reviewer_graph.py
git commit -m "feat(agent): thread an optional TranscriptWriter through the coder and reviewer graphs"
```

---

### Task 4: `worker.py` — the `--transcript` flag

**Files:**
- Modify: `agent/src/clipse_agent/worker.py`
- Test: `agent/tests/test_worker.py`

**Interfaces:**
- Produces: `--transcript` argv flag (default `""` = disabled); `_build_transcript(path: str) -> TranscriptWriter | None`; both `_dispatch`'s coder and reviewer branches gain `"transcript": _build_transcript(args.transcript)` in `extra_kwargs`.

- [ ] **Step 1: Write the failing tests**

Add `from pathlib import Path` and `from clipse_agent.transcript import TranscriptWriter` to `agent/tests/test_worker.py`'s imports, then append (near the other shell-allow-list flag tests):

```python
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd agent && uv run pytest tests/test_worker.py -k transcript -v`
Expected: every new test FAILS — `error: unrecognized arguments: --transcript=...` (argparse) or `KeyError: 'transcript'`.

- [ ] **Step 3: Implement**

In `agent/src/clipse_agent/worker.py`:

1. Add the import: `from clipse_agent.transcript import TranscriptWriter`.

2. Add the flag in `_build_parser`, right after `--docs-shell-allow-list`:

```python
    parser.add_argument("--transcript", default="")
```

3. Add the helper right after `_parse_shell`:

```python
def _build_transcript(path: str) -> TranscriptWriter | None:
    """Build this run's transcript writer, or None when disabled.

    `""` (the default, and what `internal/spawn.workerArgs` sends whenever
    `WorkerSpec.TranscriptPath` is unset -- e.g. a hand-built test config
    with no `BoardDir`) means disabled: no writer is built, and the lane's
    graph gets `transcript=None`, its own default.
    """
    return TranscriptWriter(path) if path else None
```

4. Add `"transcript": _build_transcript(args.transcript)` to both `extra_kwargs` dicts in `_dispatch`:

```python
    if lane == Lane.coder:
        return await _run_lane_graph(
            args,
            build_coder_graph,
            lane=Lane.coder,
            extra_kwargs={
                "profile": get_coder_profile(
                    args.model or None,
                    model_params=_parse_params(args.model_params),
                    shell_allow_list=_parse_shell(args.shell_allow_list),
                ),
                "docs_profile": get_coder_docs_profile(
                    args.docs_model or None,
                    model_params=_parse_params(args.docs_model_params),
                    shell_allow_list=_parse_shell(args.docs_shell_allow_list),
                ),
                "transcript": _build_transcript(args.transcript),
            },
        )
    if lane == Lane.reviewer:
        return await _run_lane_graph(
            args,
            build_reviewer_graph,
            lane=Lane.reviewer,
            extra_kwargs={
                "profile": get_reviewer_profile(
                    args.model or None,
                    model_params=_parse_params(args.model_params),
                    shell_allow_list=_parse_shell(args.shell_allow_list),
                ),
                "transcript": _build_transcript(args.transcript),
            },
        )
```

- [ ] **Step 4: Run the full worker test file**

Run: `cd agent && uv run pytest tests/test_worker.py -v`
Expected: ALL PASS.

- [ ] **Step 5: Run the full Python suite + lint**

Run: `cd agent && uv run pytest && uvx ruff check .`
Expected: 276 pass (273 from Task 3 + 3 new), no lint errors.

- [ ] **Step 6: Commit**

```bash
git add agent/src/clipse_agent/worker.py agent/tests/test_worker.py
git commit -m "feat(worker): add --transcript flag, threaded into the coder/reviewer graph kwargs"
```

---

### Task 5: Go kernel — `WorkerSpec.TranscriptPath` + `dispatcher.transcriptPath`

Mirrors `CheckpointDB`/`checkpointDBPath` byte-for-byte: an optional `WorkerSpec` field, an argv flag `internal/spawn.workerArgs` appends only when non-empty, and a `dispatcher` helper that derives the path from `cfg.BoardDir` and the issue identifier (returning `""` when `BoardDir` is unset, matching `checkpointDBPath`'s fallback for hand-built test `Config`s).

**Files:**
- Modify: `internal/spawn/spawn.go` (`WorkerSpec.TranscriptPath`)
- Modify: `internal/spawn/local.go` (`workerArgs`)
- Modify: `internal/spawn/argv_test.go`
- Modify: `dispatcher/spawn.go` (`transcriptPath`, `spawnAttempt` wiring)
- Test: `dispatcher/transcript_test.go` (new)

**Interfaces:**
- Produces: `spawn.WorkerSpec.TranscriptPath string`; `workerArgs` appends `--transcript=<value>` only when non-empty; `Dispatcher.transcriptPath(issue store.Issue) string` returns `filepath.Join(cfg.BoardDir, "logs", issue.Identifier+".transcript.jsonl")`, or `""` when `cfg.BoardDir == ""`.

- [ ] **Step 1: Write the failing argv test**

Append two cases to the `tests` table in `internal/spawn/argv_test.go`, right before the table's closing `}`:

```go
		{
			name: "transcript appended when set",
			spec: WorkerSpec{
				Lane:           "coder",
				TranscriptPath: "/board/logs/CLP-1.transcript.jsonl",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				"--transcript=/board/logs/CLP-1.transcript.jsonl",
			},
		},
		{
			name: "transcript omitted when empty",
			spec: WorkerSpec{
				Lane: "coder",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/spawn -run TestWorkerArgs -v`
Expected: FAIL on `"transcript appended when set"` — `workerArgs(...)` doesn't append `--transcript=...` (unknown field `TranscriptPath` won't even compile yet, since it doesn't exist on `WorkerSpec` — expected build error: `unknown field TranscriptPath in struct literal of type spawn.WorkerSpec`).

- [ ] **Step 3: Implement the Go side**

In `internal/spawn/spawn.go`, add to `WorkerSpec` right after `BaseBranch`:

```go
	// TranscriptPath is the absolute path to this issue's per-ticket agent
	// transcript JSONL file -- one file per issue, accumulating across every
	// turn/lane/rework it ever runs (see AGENTS.md's transcript bullet and
	// dispatcher.transcriptPath). Optional: LocalSpawner appends
	// --transcript=<value> only when this is non-empty, so a hand-built
	// WorkerSpec with no board directory to root a path under (most kernel
	// tests) simply omits the flag and the worker runs with transcripts
	// disabled.
	TranscriptPath string
```

In `internal/spawn/local.go`, replace `workerArgs`'s doc comment (the whole comment block immediately above `func workerArgs`) with:

```go
// workerArgs returns the ordered CLI flags every worker invocation carries:
// the five fields every worker invocation carries, followed by
// --checkpoint-db, --max-tokens, --model, --docs-model, --model-params,
// --docs-model-params, --shell-allow-list, --docs-shell-allow-list,
// --base-branch, and --transcript ONLY when spec carries them (CheckpointDB
// non-empty / MaxTokens > 0 / Model non-empty / DocsModel non-empty /
// ModelParams non-empty / DocsModelParams non-empty / ShellAllowList
// non-empty / DocsShellAllowList non-empty / BaseBranch non-empty /
// TranscriptPath non-empty — see WorkerSpec's doc comment).
// Kept as a pure helper (tested directly in argv_test.go) so this
// conditional-append logic doesn't need a real subprocess to exercise, and
// so a worker that has none of these configured (e.g. testworker, driven by
// hand-built WorkerSpecs in kernel tests) never sees a flag it doesn't
// understand.
```

Then append the conditional right after the `BaseBranch` one inside the function body:

```go
	if spec.TranscriptPath != "" {
		args = append(args, "--transcript="+spec.TranscriptPath)
	}
```

- [ ] **Step 4: Argv test green**

Run: `go test ./internal/spawn -run TestWorkerArgs -v`
Expected: PASS.

- [ ] **Step 5: Write the failing dispatcher wiring tests**

Create `dispatcher/transcript_test.go`:

```go
package dispatcher_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/linear"
)

// TestSpawnAttempt_WiresTranscriptPath asserts a claimed issue's spawned
// worker gets a --transcript path derived from cfg.BoardDir (one file per
// issue, named by the issue's Linear identifier, living in <board_dir>/logs
// next to the per-issue stderr log -- AGENTS.md's transcript bullet), the
// same direct cfg-to-spec forwarding as CheckpointDB/MaxTokens/BaseBranch
// (see checkpoint_test.go).
func TestSpawnAttempt_WiresTranscriptPath(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.BoardDir = "/tmp/clipse-board"

	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}

	wantTranscript := filepath.Join(cfg.BoardDir, "logs", "issue-1.transcript.jsonl")
	if specs[0].TranscriptPath != wantTranscript {
		t.Errorf("TranscriptPath = %q, want %q", specs[0].TranscriptPath, wantTranscript)
	}
}

// TestSpawnAttempt_TranscriptPathEmptyWhenBoardDirUnset asserts a Config
// with no BoardDir configured (the zero value -- e.g. a hand-built test
// Config that never went through config.Load, as most dispatcher tests
// use) produces an empty WorkerSpec.TranscriptPath rather than a
// nonsensical path rooted at "", so LocalSpawner omits --transcript
// entirely (see internal/spawn.workerArgs) and the worker runs with
// transcripts disabled. Real production configs always have a non-empty
// BoardDir (config.Load defaults it).
func TestSpawnAttempt_TranscriptPathEmptyWhenBoardDirUnset(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig() // BoardDir left at zero value.

	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}
	if specs[0].TranscriptPath != "" {
		t.Errorf("TranscriptPath = %q, want empty (BoardDir unset)", specs[0].TranscriptPath)
	}
}
```

- [ ] **Step 6: Run to verify failure**

Run: `go test ./dispatcher -run TestSpawnAttempt_WiresTranscriptPath -v`
Expected: FAIL — `specs[0].TranscriptPath` is `""`, want the derived path (compiles fine; `transcriptPath` doesn't exist yet, but the test itself doesn't call it directly, only through `spawnAttempt`'s currently-unwired `WorkerSpec` literal, so the failure is an assertion mismatch, not a build error).

- [ ] **Step 7: Implement the dispatcher side**

In `dispatcher/spawn.go`, add `transcriptPath` right after `checkpointDBPath`:

```go
// transcriptPath returns the per-issue agent transcript JSONL path the
// worker should append every DAC turn/tool event to, derived from
// cfg.BoardDir and the issue's Linear identifier -- one file per issue,
// living next to the per-issue stderr log LocalSpawner already writes to
// <board_dir>/logs/<issue>.log (see internal/spawn/local.go's
// stderrLogPath), so every turn/lane/rework this issue ever runs
// accumulates into the SAME file (AGENTS.md's transcript bullet). Returns
// "" when BoardDir is unset -- mirrors checkpointDBPath's own "no directory
// to root a path under" fallback for hand-built Configs that bypass
// config.Load (most dispatcher tests); LocalSpawner only appends
// --transcript when this is non-empty (see internal/spawn.workerArgs). Real
// production configs always have a non-empty BoardDir (config.Load defaults
// it), so the transcript is always-on there.
func (d *Dispatcher) transcriptPath(issue store.Issue) string {
	if d.cfg.BoardDir == "" {
		return ""
	}
	return filepath.Join(d.cfg.BoardDir, "logs", issue.Identifier+".transcript.jsonl")
}
```

Add `TranscriptPath: d.transcriptPath(issue),` to `spawnAttempt`'s `spawn.WorkerSpec{...}` literal, right after `BaseBranch: d.cfg.Repo.BaseBranch,`:

```go
	spec := spawn.WorkerSpec{
		Issue:              issue.Identifier,
		Lane:               lane,
		RunID:              runID,
		ThreadID:           threadID,
		Workspace:          workspace,
		Env:                env,
		CheckpointDB:       d.checkpointDBPath(issue),
		MaxTokens:          d.cfg.MaxTokensPerRun,
		Model:              model,
		DocsModel:          docsModel,
		ModelParams:        modelParams,
		DocsModelParams:    docsModelParams,
		ShellAllowList:     shellAllowList,
		DocsShellAllowList: docsShellAllowList,
		BaseBranch:         d.cfg.Repo.BaseBranch,
		TranscriptPath:     d.transcriptPath(issue),
	}
```

- [ ] **Step 8: Full Go suite**

Run: `go test ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/spawn/spawn.go internal/spawn/local.go internal/spawn/argv_test.go dispatcher/spawn.go dispatcher/transcript_test.go
git commit -m "feat(spawn): thread a per-issue --transcript path from board_dir to the worker"
```

---

### Task 6: Eval harness touch + AGENTS.md

**Files:**
- Modify: `agent/evals/harness.py` (`run_coder_turn`)
- Modify: `agent/evals/test_coder_evals.py` (C1)
- Modify: `AGENTS.md`

**Interfaces:**
- Produces: `harness.run_coder_turn(..., transcript_path: str = "")` — `""` (default) disables the transcript, matching every other layer's convention.

- [ ] **Step 1: Thread `transcript_path` through the harness**

In `agent/evals/harness.py`, add the import `from clipse_agent.transcript import TranscriptWriter`, and change `run_coder_turn`:

```python
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
```

- [ ] **Step 2: Extend C1 with a transcript assertion**

This re-runs C1's SAME live coder turn — no extra token cost, just a few more assertions against the file that turn's own graph already writes when a path is given.

In `agent/evals/test_coder_evals.py`, add `import json` to the imports, and change `test_c1_smoke_small_fix`:

```python
def test_c1_smoke_small_fix(tmp_path: Path, eval_env: Path, record_result) -> None:
    repo = make_fixture_repo(
        tmp_path,
        files={
            "calc.py": _CALC_BUGGY,
            "README.md": "# calc\n`total(xs)` sums a list.\n",
        },
    )
    transcript_path = tmp_path / "calc.transcript.jsonl"
    result = run_coder_turn(
        repo,
        "EVAL-1: total() returns the wrong sum.\n\n"
        "`calc.total([1, 2, 3])` returns 3, expected 6 — the loop drops the "
        "last element. Fix `total` in calc.py so it sums every element.",
        transcript_path=str(transcript_path),
    )
    record_result(result)

    assert result.outcome == Outcome.needs_review
    assert result.pr_url == "https://github.example/fake/pull/1"
    assert (eval_env / "pr.json").exists()
    assert _branch_commits(repo) >= 1
    # The fix actually works.
    check = subprocess.run(_CALC_FIXED_CHECK, cwd=repo.worktree, capture_output=True, text=True)
    assert check.returncode == 0, check.stderr
    # The graph's commit-message contract held (issue-id prefix from the tail's TITLE).
    subject = git_out(repo.worktree, "log", "-1", "--format=%s")
    assert subject.startswith("EVAL-1:")
    # The branch was actually pushed.
    assert git_out(repo.worktree, "rev-parse", "HEAD") == git_out(
        repo.worktree, "rev-parse", f"origin/{repo.branch}"
    )
    # Transcript logging (clipse feature D): the coding turn's own graph
    # wrote a real transcript file with at least one full turn's worth of
    # events, straight from the live model's real tool use.
    events = [json.loads(line) for line in transcript_path.read_text().splitlines() if line.strip()]
    assert sum(1 for e in events if e["event"] == "turn_start") >= 1
    assert sum(1 for e in events if e["event"] == "turn_end") >= 1
    assert sum(1 for e in events if e["event"] == "tool_call") >= 1
    assert all(e["lane"] == "coder" for e in events if e["event"] in ("turn_start", "turn_end"))
```

- [ ] **Step 3: Run the harness self-test + (if credentials are available) C1 live**

Run: `cd agent && uv run pytest evals/test_harness_selftest.py -v`
Expected: 3 PASS (no LLM involved; proves the harness/import wiring alone is sound).

Run (only with `ANTHROPIC_API_KEY` sourced): `source ~/.secrets && cd agent && uv run pytest evals/test_coder_evals.py::test_c1_smoke_small_fix -v`
Expected: PASS, in a few minutes, same as before this task — plus the new transcript assertions passing against the real file the live turn wrote.

- [ ] **Step 4: Document the feature in AGENTS.md**

Add a bullet to AGENTS.md's "Conventions" section, after the `model_params` precedence/effort bullet:

```markdown
- **Per-ticket transcript logging**: every coder/coder_docs/reviewer DAC turn
  appends one JSON-object-per-line to `<board_dir>/logs/<ISSUE>.transcript.jsonl`
  (one file per issue, accumulating across every turn/lane/rework it ever
  runs) via `clipse_agent.transcript.TranscriptWriter`, tapped inside
  `dac.drive_turn`'s stream loop through an optional `event_sink`. Event
  types: `turn_start`/`turn_end` (lane, run_id, thread_id, assistant_id,
  model, plus task_text/outcome+tokens respectively), `assistant` (text),
  `tool_call` (name, args), `tool_result` (name, status, content truncated to
  8k chars), `interrupt` (payload repr). A write failure is logged to stderr
  and swallowed -- the transcript is a debug aid, never load-bearing for a
  run's outcome. Threaded end to end exactly like the checkpoint DB path:
  `dispatcher.transcriptPath` (derived from `cfg.BoardDir`) ->
  `WorkerSpec.TranscriptPath` -> `--transcript=` argv -> the worker's
  `TranscriptWriter`; disabled (`None`) whenever the flag is omitted, which
  is every hand-built test config that has no `BoardDir` to root a path
  under.
```

- [ ] **Step 5: Full gates**

Run, in order:

```bash
make test
make lint
```

Expected: both green — `test-go` unaffected, `test-py` at 276 tests (unchanged from Task 4; Task 6 only modified two eval files, which `testpaths = ["tests"]` never collects for `make test`), `lint` clean.

- [ ] **Step 6: Commit**

```bash
git add agent/evals/harness.py agent/evals/test_coder_evals.py AGENTS.md
git commit -m "feat(evals): thread transcript logging through the coder eval harness, document in AGENTS.md"
```

---

## Self-Review

- **Streamed tool-call merging (`AIMessageChunk.__add__`) is verified against the installed `langchain_core` version in this repo's `agent/.venv`, not against every provider/version combination clipse might run in production.** The REPL snippet in Task 2 is real output from this repo's venv, and `add_ai_message_chunks` is a stable, documented LangChain mechanism (it's how LangChain's own streaming examples reassemble tool calls), so I'm fairly confident — but I have not driven a REAL multi-chunk Anthropic/OpenAI tool-call stream through `drive_turn` end-to-end; the eval touch in Task 6 (C1's `tool_call` count assertion) is the first point this actually gets proven live, and it's plausible the merge needs a tweak once it sees real provider output (e.g. if a provider streams tool calls only as one atomic chunk, in which case the merge logic is simply inert — not wrong, just unexercised).
- **`event_sink` scope on the exception path**: I deliberately chose NOT to emit `turn_end` (or a `turn_error` event) when `drive_turn`'s `except Exception` branch raises `DacError`. The design brief didn't mention an error event type, and adding one would mean deciding where a `DacError` should be caught to still let a `finally`-guaranteed transcript write happen without changing the exception's propagation — out of scope as I read the brief, but worth confirming: should a crashed turn still leave a transcript trace (e.g. `turn_end` with an `error` field) so the file doesn't just go silent mid-run?
- **`assistant`/`tool_call` events are flushed per logical message, not per raw chunk** — i.e., roughly once per LLM turn-segment rather than once per token. This was a deliberate reading of "one event per line" (a token-by-token dump would be enormous and useless to a human reader), but it's an interpretation, not something the design brief stated explicitly either way.
- **No cap on `assistant` event text size** — only `tool_result.content` is truncated (8k chars), matching the design brief's wording exactly ("tool_result ... content truncated to ~8k chars/event"). A single huge narration block (rare, since the structured tail keeps messages short) would ride through uncapped. Flagging in case the intent was actually "truncate every event's biggest text field," not just tool_result's.
- **Go test count / exact final Python test counts in each task's "Expected" line are computed from the current baseline (259) plus the tests I planned to add** (9 + 5 + 3 = 276 total by Task 4; Task 6 adds none to `tests/`). If an implementer's actual test count drifts (e.g. splits a test I wrote as one into two), these numbers are a sanity check, not a hard gate — the real gate is "ALL PASS, zero unexplained deltas."
- **I did not re-verify `ToolMessage.status`'s exact default value** (confirmed the field exists on the model, and that constructing one with an explicit `status="success"` round-trips) — Task 2's test passes it explicitly rather than relying on the default, so this shouldn't matter, but a `ToolMessage` built by DAC's own tool-calling middleware might set `status` to something other than `"success"`/`"error"` in a way I haven't seen firsthand.
- **`internal/spawn/local_test.go`** (the exec-plumbing-level test file, distinct from `argv_test.go`) was not touched — `TranscriptPath` only affects `workerArgs`' pure argv-building logic, which `argv_test.go` already covers directly; I didn't see a need to also exercise it through a real `exec.Command` the way `local_test.go` does for the flag set as a whole, but a stricter reviewer might want one combined-flags case there too.

## Controller amendments (2026-07-07, resolve the Self-Review questions)

1. **Crashed turns DO leave a trace**: in `drive_turn`'s `except Exception` branch (the one that raises `DacError`), best-effort emit `{"type": "turn_end", "error": str(exc)}` via the sink before raising — the sink never raises, so this cannot mask the original exception. Add one unit test: a turn_driver stream that raises mid-stream still yields a turn_end event carrying the error field. Postmortems are the feature's whole point; a transcript that goes silent exactly when things break defeats it.
2. **Cap assistant text too**: apply the same 8k-char truncation to `assistant` event text as to `tool_result` content (single shared constant). One assertion added to the existing writer/tap tests.
3. Per-logical-message event granularity (not per-token) is CONFIRMED as intended. `local_test.go` combined-flags case not required. Expected test-count lines are sanity checks, not gates — implementers report actual counts.
