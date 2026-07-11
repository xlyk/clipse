"""Typed contracts and secret-safe host GitHub authentication."""

from __future__ import annotations

import json
import subprocess

import pytest

from clipse_agent.backends.contracts import (
    BackendActionRequest,
    BackendActionResult,
    BackendWorkspace,
)
from clipse_agent.backends.daytona import labels_for, owner_key, repo_label
from clipse_agent.backends.github import (
    BackendActionError,
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
        id="sandbox-1",
        state="started",
        path="repo",
        owner="daytona:xlyk/clipse:coder:issue-1",
    )
    result = BackendActionResult(
        action="ensure",
        provider="daytona",
        ok=True,
        workspace=workspace,
    )

    dumped = json.loads(result.model_dump_json(exclude_none=True))

    assert dumped["workspace"]["id"] == "sandbox-1"
    assert "error_kind" not in dumped
    assert "error_operation" not in dumped
    assert "error_message" not in dumped


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
