from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import sys
from types import ModuleType

import pytest

from clipse_agent.backends.contracts import BackendActionRequest, BackendWorkspace
from clipse_agent.backends.session import CommandResult


def _smoke_module() -> ModuleType:
    path = Path(__file__).parents[2] / "scripts" / "smoke_daytona_backend.py"
    spec = importlib.util.spec_from_file_location("smoke_daytona_backend", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def _request(*, role: str, run_id: str) -> BackendActionRequest:
    return BackendActionRequest(
        action="ensure",
        provider="daytona",
        repo_slug="xlyk/clipse",
        repo_url="https://github.com/xlyk/clipse.git",
        base_branch="main",
        branch="smoke/daytona-1",
        issue_id="smoke-daytona-1",
        run_id=run_id,
        role=role,
        auto_stop_minutes=60,
        reviewer_auto_delete_minutes=60,
    )


def test_cleanup_github_recovers_lost_create_close_push_and_delete_responses() -> None:
    smoke = _smoke_module()
    branch = "smoke/daytona-1"
    pr_url = "https://github.com/xlyk/clipse/pull/123"
    pr_observations = iter([[], [pr_url], [], []])
    branch_exists = True
    close_calls: list[str] = []
    delete_calls: list[str] = []

    def run_host(argv: list[str]) -> str:
        nonlocal branch_exists
        if argv[:3] == ["gh", "pr", "list"]:
            return json.dumps([{"url": url} for url in next(pr_observations, [])])
        if argv[:3] == ["gh", "pr", "close"]:
            close_calls.append(argv[3])
            raise smoke.SmokeError("lost PR close response")
        if argv[:3] == ["gh", "api", "-X"]:
            delete_calls.append(argv[-1])
            branch_exists = False
            raise smoke.SmokeError("lost branch delete response")
        if argv[:2] == ["gh", "api"]:
            return json.dumps([{"ref": f"refs/heads/{branch}"}] if branch_exists else [])
        raise AssertionError(argv)

    smoke.cleanup_github(
        run_host,
        "xlyk/clipse",
        branch,
        attempts=3,
        sleep=lambda _: None,
    )

    assert close_calls == [pr_url]
    assert len(delete_calls) == 1


@pytest.mark.parametrize(
    "payload",
    [
        {},
        "not-a-list",
        [1],
        [{}],
        [{"url": ""}],
        [{"url": "   "}],
        [{"url": 42}],
        [{"url": "https://github.com/xlyk/clipse/pull/123"}, {}],
    ],
)
def test_open_pr_urls_rejects_wrong_or_malformed_response_shape(payload: object) -> None:
    smoke = _smoke_module()

    with pytest.raises(smoke.SmokeError):
        smoke._open_pr_urls(lambda _argv: json.dumps(payload), "xlyk/clipse", "smoke/daytona-1")


@pytest.mark.parametrize(
    "payload",
    [
        {},
        "not-a-list",
        [1],
        [{}],
        [{"ref": ""}],
        [{"ref": "   "}],
        [{"ref": 42}],
        [{"ref": "refs/heads/smoke/daytona-1"}, {}],
    ],
)
def test_branch_refs_rejects_wrong_or_malformed_response_shape(payload: object) -> None:
    smoke = _smoke_module()

    with pytest.raises(smoke.SmokeError):
        smoke._branch_refs(lambda _argv: json.dumps(payload), "xlyk/clipse", "smoke/daytona-1")


def test_cleanup_sandboxes_rediscovers_lost_create_and_retries_lost_delete_response() -> None:
    smoke = _smoke_module()
    workspace = BackendWorkspace(
        external_id="sb-smoke",
        state="active",
        workspace_path="/remote/repo",
        owner_key="daytona:xlyk/clipse:coder:smoke-daytona-1",
    )
    observations = iter([[], [workspace], [workspace], []])

    class Lifecycle:
        def __init__(self) -> None:
            self.deleted: list[str] = []

        def list(self, _request: BackendActionRequest) -> list[BackendWorkspace]:
            return next(observations, [])

        def delete(self, request: BackendActionRequest) -> BackendWorkspace:
            assert request.sandbox_id is not None
            self.deleted.append(request.sandbox_id)
            if len(self.deleted) == 1:
                raise smoke.SmokeError("lost sandbox delete response")
            return workspace.model_copy(update={"state": "deleted"})

    lifecycle = Lifecycle()
    request = _request(role="coder", run_id="coder-1")
    list_request = BackendActionRequest(action="list", provider="daytona", repo_slug="xlyk/clipse")

    smoke.cleanup_sandboxes(
        lifecycle,
        list_request,
        [request],
        "smoke-daytona-1",
        known=[],
        attempts=3,
        sleep=lambda _: None,
    )

    assert lifecycle.deleted == ["sb-smoke", "sb-smoke"]


class _ReviewerSession:
    def __init__(self, branch: str, marker: str) -> None:
        self.branch = branch
        self.marker = marker

    def run(self, argv: list[str]) -> CommandResult:
        if argv == ["git", "branch", "--show-current"]:
            return CommandResult(0, stdout=f"{self.branch}\n")
        if argv[:1] == ["cat"]:
            return CommandResult(0, stdout=f"{self.marker}\n")
        return CommandResult(1, stderr="unexpected command")


def test_reviewer_proof_requires_remote_state_tool_call_and_anchored_verdict() -> None:
    smoke = _smoke_module()
    session = _ReviewerSession("smoke/daytona-1", "marker-1")
    smoke.verify_reviewer_remote_state(session, "smoke/daytona-1", "marker.txt", "marker-1")

    evidence = smoke.AgentTurnEvidence(
        final_text="Review complete\nVERDICT: PASS",
        tool_calls=("shell",),
    )
    assert smoke.validate_reviewer_evidence(evidence) == "PASS"


def test_reviewer_proof_accepts_pinned_dac_execute_tool() -> None:
    smoke = _smoke_module()
    evidence = smoke.AgentTurnEvidence(
        final_text="VERDICT: CHANGES_REQUESTED",
        tool_calls=("execute",),
    )

    assert smoke.validate_reviewer_evidence(evidence) == "CHANGES_REQUESTED"


@pytest.mark.parametrize(
    ("text", "tools"),
    [
        ("VERDICT: PASS", ()),
        ("VERDICT: PASS because it is small", ("shell",)),
        ("VERDICT: MAYBE", ("shell",)),
        ("VERDICT: PASS\nVERDICT: CHANGES_REQUESTED", ("shell",)),
    ],
)
def test_reviewer_proof_rejects_host_diff_only_or_invalid_verdict(
    text: str,
    tools: tuple[str, ...],
) -> None:
    smoke = _smoke_module()

    with pytest.raises(smoke.SmokeError):
        smoke.validate_reviewer_evidence(smoke.AgentTurnEvidence(text, tools))


def test_reviewer_remote_state_rejects_wrong_branch_or_marker() -> None:
    smoke = _smoke_module()

    with pytest.raises(smoke.SmokeError):
        smoke.verify_reviewer_remote_state(
            _ReviewerSession("main", "wrong"),
            "smoke/daytona-1",
            "marker.txt",
            "marker-1",
        )
