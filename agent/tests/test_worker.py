"""Tests for the clipse-worker stub entrypoint.

The worker's job is to print exactly one line of JSON to stdout that
round-trips cleanly through the generated WorkerResult pydantic model.
The dispatcher is the only consumer of this contract.
"""

import subprocess
import sys

from clipse_agent.contract import WorkerResult
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
    assert result.block_kind is None
    assert result.lane == "coder"


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
