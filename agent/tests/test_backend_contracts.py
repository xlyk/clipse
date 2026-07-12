"""Typed contracts and secret-safe host GitHub authentication."""

from __future__ import annotations

import json
import subprocess

import pytest
from pydantic import ValidationError

from clipse_agent.backends.contracts import (
    BackendActionRequest,
    BackendActionResult,
    BackendWorkspace,
)
from clipse_agent.backends.daytona import labels_for, owner_key, repo_label
from clipse_agent.backends.github import (
    BackendActionError,
    github_auth_preflight,
    github_branch_exists,
    github_token,
    safe_error,
    subprocess_host_runner,
)


def _request(**overrides: object) -> BackendActionRequest:
    values: dict[str, object] = {
        "action": "ensure",
        "provider": "daytona",
        "repo_url": "https://github.com/xlyk/clipse.git",
        "repo_slug": "xlyk/clipse",
        "base_branch": "main",
        "branch": "feat/CLI-1",
        "issue_id": "issue-1",
        "run_id": "run-1",
        "role": "coder",
        "auto_stop_minutes": 60,
        "reviewer_auto_delete_minutes": 60,
    }
    values.update(overrides)
    return BackendActionRequest(**values)


def test_coder_owner_and_labels_are_stable() -> None:
    request = _request()

    assert owner_key(request) == "daytona:xlyk/clipse:coder:issue-1"
    assert labels_for(request) == {
        "created-by": "clipse",
        "repo": repo_label("xlyk/clipse"),
        "issue": "issue-1",
        "role": "coder",
    }


def test_reviewer_owner_and_labels_are_scoped_to_the_run() -> None:
    request = _request(role="reviewer")

    assert owner_key(request) == "daytona:xlyk/clipse:reviewer:issue-1:run-1"
    assert labels_for(request) == {
        "created-by": "clipse",
        "repo": repo_label("xlyk/clipse"),
        "issue": "issue-1",
        "role": "reviewer",
        "run": "run-1",
    }


def test_success_result_omits_absent_error_fields() -> None:
    workspace = BackendWorkspace(
        external_id="sandbox-1",
        state="active",
        workspace_path="/home/daytona/workspace/clipse",
        owner_key="daytona:xlyk/clipse:coder:issue-1",
    )
    result = BackendActionResult(
        action="ensure",
        provider="daytona",
        ok=True,
        **workspace.model_dump(),
    )

    dumped = json.loads(result.model_dump_json(exclude_none=True))

    assert dumped["external_id"] == "sandbox-1"
    assert dumped["owner_key"] == "daytona:xlyk/clipse:coder:issue-1"
    assert dumped["workspace_path"] == "/home/daytona/workspace/clipse"
    assert dumped["state"] == "active"
    assert "workspace" not in dumped
    assert "error_kind" not in dumped
    assert "error_operation" not in dumped
    assert "error" not in dumped


def test_repo_label_hashes_normalized_identity_without_sanitizer_collisions() -> None:
    assert repo_label(" XLYK/CLIPSE.git ") == repo_label("xlyk/clipse")
    assert repo_label("xlyk/clipse") != repo_label("xlyk-clipse")
    assert len(repo_label("xlyk/clipse")) == 64


def test_list_request_needs_repository_identity_only() -> None:
    request = BackendActionRequest(action="list", provider="daytona", repo_slug="xlyk/clipse")

    assert request.issue_id is None
    assert request.run_id is None
    assert request.role is None


@pytest.mark.parametrize(
    "overrides",
    [
        {"repo_slug": ""},
        {"issue_id": ""},
        {"auto_stop_minutes": 0},
        {"reviewer_auto_delete_minutes": -1},
    ],
)
def test_ensure_request_rejects_empty_identity_or_nonpositive_lifecycle_values(overrides: dict[str, object]) -> None:
    with pytest.raises(ValidationError):
        _request(**overrides)


def test_ensure_request_requires_scoped_fields() -> None:
    with pytest.raises(ValidationError):
        BackendActionRequest(action="ensure", provider="daytona", repo_slug="xlyk/clipse")


@pytest.mark.parametrize("role", ["coder", "reviewer"])
def test_ensure_request_requires_run_id_for_every_role(role: str) -> None:
    with pytest.raises(ValidationError):
        _request(role=role, run_id=None)


def test_delete_request_requires_scope_and_sandbox_id() -> None:
    with pytest.raises(ValidationError):
        BackendActionRequest(
            action="delete",
            provider="daytona",
            repo_slug="xlyk/clipse",
            issue_id="issue-1",
            run_id="run-1",
            role="coder",
        )


def test_coder_delete_does_not_require_run_id() -> None:
    request = BackendActionRequest(
        action="delete",
        provider="daytona",
        repo_slug="xlyk/clipse",
        issue_id="issue-1",
        role="coder",
        sandbox_id="sandbox-1",
    )

    assert request.run_id is None


def test_reviewer_delete_requires_run_id() -> None:
    with pytest.raises(ValidationError):
        BackendActionRequest(
            action="delete",
            provider="daytona",
            repo_slug="xlyk/clipse",
            issue_id="issue-1",
            role="reviewer",
            sandbox_id="sandbox-1",
        )


@pytest.mark.parametrize(
    "values",
    [
        {
            "action": "ensure",
            "provider": "daytona",
            "ok": True,
            "owner_key": "daytona:xlyk/clipse:coder:issue-1",
            "external_id": "sandbox-1",
            "workspace_path": "/home/daytona/workspace/clipse",
            "state": "active",
            "error_kind": "transient",
            "error": "failed",
        },
        {"action": "ensure", "provider": "daytona", "ok": False},
        {
            "action": "ensure",
            "provider": "daytona",
            "ok": False,
            "error_kind": "transient",
            "error": "failed",
        },
        {
            "action": "ensure",
            "provider": "daytona",
            "ok": False,
            "error_kind": "transient",
            "error": "failed",
            "external_id": "sandbox-1",
        },
    ],
)
def test_result_rejects_mixed_or_incomplete_success_and_error_states(values: dict[str, object]) -> None:
    with pytest.raises(ValidationError):
        BackendActionResult(**values)


def test_workspace_rejects_raw_provider_state() -> None:
    with pytest.raises(ValidationError):
        BackendWorkspace(
            external_id="sandbox-1",
            state="started",
            workspace_path="/home/daytona/workspace/clipse",
            owner_key="daytona:xlyk/clipse:coder:issue-1",
        )


def test_safe_error_never_echoes_token_looking_exception_text() -> None:
    token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
    exc = RuntimeError(f"clone rejected https://x-access-token:{token}@github.com/xlyk/clipse.git")

    message = safe_error("clone", exc)

    assert token not in message
    assert "x-access-token" not in message
    assert message == "clone failed (RuntimeError)"


def test_github_token_checks_auth_then_reads_token() -> None:
    calls: list[list[str]] = []

    def runner(argv: list[str]) -> str:
        calls.append(argv)
        return "" if "status" in argv else "secret-token"

    assert github_token(runner) == "secret-token"
    assert calls == [
        ["gh", "auth", "status", "--hostname", "github.com"],
        ["gh", "auth", "token", "--hostname", "github.com"],
    ]


def test_github_auth_preflight_checks_status_without_materializing_token() -> None:
    calls: list[list[str]] = []

    github_auth_preflight(lambda argv: calls.append(argv) or "")

    assert calls == [["gh", "auth", "status", "--hostname", "github.com"]]


def test_github_token_empty_result_is_needs_input() -> None:
    with pytest.raises(BackendActionError) as raised:
        github_token(lambda _argv: "")

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "github_auth"
    assert str(raised.value) == "gh auth token returned empty"


def test_subprocess_host_runner_discards_sensitive_process_output(monkeypatch) -> None:
    token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

    def fail(*_args: object, **_kwargs: object) -> subprocess.CompletedProcess[str]:
        raise subprocess.CalledProcessError(
            17,
            ["gh", "auth", "token", "--hostname", "github.com"],
            output=token,
            stderr=f"authorization: bearer {token}",
        )

    monkeypatch.setattr(subprocess, "run", fail)

    with pytest.raises(BackendActionError) as raised:
        subprocess_host_runner(["gh", "auth", "token", "--hostname", "github.com"])

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "gh auth token"
    assert str(raised.value) == "gh auth token exited with status 17"
    assert token not in str(raised.value)


def test_subprocess_host_runner_preserves_only_safe_no_pr_signal(monkeypatch) -> None:
    token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

    def fail(*_args: object, **_kwargs: object) -> subprocess.CompletedProcess[str]:
        raise subprocess.CalledProcessError(
            1,
            ["gh", "pr", "view", "feat/CLI-1", "--repo", "xlyk/clipse"],
            stderr=f"no pull requests found for branch; authorization: bearer {token}",
        )

    monkeypatch.setattr(subprocess, "run", fail)

    with pytest.raises(BackendActionError) as raised:
        subprocess_host_runner(["gh", "pr", "view", "feat/CLI-1", "--repo", "xlyk/clipse"])

    assert str(raised.value) == "no pull requests found"
    assert token not in str(raised.value)


@pytest.mark.parametrize("payload", [{"ref": "refs/heads/feat/CLI-1"}, [{}], [{"ref": 7}]])
def test_github_branch_exists_rejects_malformed_success_payload(monkeypatch, payload: object) -> None:
    monkeypatch.setattr(
        subprocess,
        "run",
        lambda *_args, **_kwargs: subprocess.CompletedProcess([], 0, stdout=json.dumps(payload), stderr=""),
    )

    with pytest.raises(BackendActionError) as raised:
        github_branch_exists("xlyk/clipse", "feat/CLI-1")

    assert raised.value.kind == "transient"
