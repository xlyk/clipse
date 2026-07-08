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
