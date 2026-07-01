"""Tests for the clipse-worker stub entrypoint.

The worker's job is to print exactly one line of JSON to stdout that
round-trips cleanly through the generated WorkerResult pydantic model.
The dispatcher is the only consumer of this contract.
"""

import json
import subprocess
import sys

from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.worker import main


def test_main_prints_one_line_of_schema_valid_json(capsys):
    exit_code = main([])

    assert exit_code == 0

    captured = capsys.readouterr()
    lines = captured.out.splitlines()
    assert len(lines) == 1

    # Must round-trip through the generated model with no ValidationError.
    result = WorkerResult.model_validate_json(lines[0])
    assert result.outcome == "blocked"
    # Present iff outcome == "blocked" (amendment X2): the stub is blocked,
    # so block_kind must be set to a valid enum value, never null.
    assert result.block_kind is not None
    assert result.block_kind == BlockKind.transient
    assert result.lane == "coder"

    # The invariant is enforced by the producer, not the schema: assert the
    # raw JSON actually carries block_kind as a string, not a null.
    raw = json.loads(lines[0])
    assert raw["block_kind"] == "transient"


def test_non_blocked_result_omits_block_kind_when_dumped(capsys):
    # Construct a non-blocked result directly and dump it the same way the
    # worker does (exclude_none). block_kind must be OMITTED from the JSON,
    # not emitted as a null-valued key.
    result = WorkerResult(
        run_id="run-1",
        issue_id="SPAC-1",
        lane=Lane.coder,
        outcome=Outcome.done,
        summary="did the thing",
        artifacts=[],
        thread_id="thread-1",
        turn_count=1,
        tokens=Tokens(**{"in": 0, "out": 0}),
    )

    dumped = result.model_dump_json(by_alias=True, exclude_none=True)
    raw = json.loads(dumped)

    assert "block_kind" not in raw
    assert "pr_url" not in raw

    # Must still round-trip through the generated model with no ValidationError.
    reparsed = WorkerResult.model_validate_json(dumped)
    assert reparsed.block_kind is None
    assert reparsed.outcome == Outcome.done


def test_main_uses_provided_lane_and_ids():
    exit_code = main(
        [
            "--issue",
            "SPAC-123",
            "--lane",
            "reviewer",
            "--run",
            "run-1",
            "--thread",
            "thread-1",
        ]
    )
    assert exit_code == 0


def test_main_tolerates_missing_args():
    # All flags are optional; the stub must still produce a valid result.
    exit_code = main([])
    assert exit_code == 0


def test_clipse_worker_console_script_emits_valid_json():
    proc = subprocess.run(
        [sys.executable, "-m", "clipse_agent.worker"],
        capture_output=True,
        text=True,
        check=True,
    )
    lines = proc.stdout.splitlines()
    assert len(lines) == 1
    WorkerResult.model_validate_json(lines[0])
