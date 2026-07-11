from __future__ import annotations

import importlib.util
from pathlib import Path
from types import ModuleType

from clipse_agent.backends.contracts import BackendWorkspace


def _smoke_module() -> ModuleType:
    path = Path(__file__).parents[2] / "scripts" / "smoke_daytona_backend.py"
    spec = importlib.util.spec_from_file_location("smoke_daytona_backend", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_wait_for_no_leftovers_polls_until_deleted_objects_disappear() -> None:
    smoke = _smoke_module()
    workspace = BackendWorkspace(
        external_id="sb-smoke",
        state="deleted",
        workspace_path="/remote/repo",
        owner_key="daytona:xlyk/clipse:coder:smoke-daytona-1",
    )
    observations = iter([[workspace], [workspace], []])
    sleeps: list[float] = []

    smoke.wait_for_no_leftovers(
        lambda: next(observations),
        "smoke-daytona-1",
        attempts=3,
        sleep=sleeps.append,
    )

    assert sleeps == [1, 1]
