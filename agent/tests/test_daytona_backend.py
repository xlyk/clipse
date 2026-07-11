"""Fake-backed tests for the Daytona sandbox lifecycle."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from clipse_agent.backends.contracts import BackendActionRequest
from clipse_agent.backends.daytona import (
    REMOTE_REPO_REL,
    BackendActionError,
    DaytonaLifecycle,
    labels_for,
    owner_key,
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
        "reviewer_auto_delete_minutes": 45,
        "snapshot": "clipse-snapshot",
        "target": "us",
    }
    values.update(overrides)
    return BackendActionRequest(**values)


@dataclass
class _FakeGit:
    clone_calls: list[dict[str, Any]] = field(default_factory=list)
    config: dict[str, str] = field(default_factory=dict)

    def clone(self, **kwargs: Any) -> None:
        self.clone_calls.append(kwargs)


@dataclass
class _FakeSandbox:
    id: str
    state: str = "started"
    labels: dict[str, str] = field(default_factory=dict)
    env: dict[str, str] = field(default_factory=dict)
    git: _FakeGit = field(default_factory=_FakeGit)


class _FakeClient:
    def __init__(self, sandboxes: list[_FakeSandbox] | None = None) -> None:
        self.sandboxes = list(sandboxes or [])
        self.list_queries: list[Any] = []
        self.create_params: list[Any] = []
        self.started: list[_FakeSandbox] = []
        self.deleted: list[_FakeSandbox] = []
        self.gotten: list[str] = []

    def list(self, query: Any) -> list[_FakeSandbox]:
        self.list_queries.append(query)
        return [
            sandbox
            for sandbox in self.sandboxes
            if all(sandbox.labels.get(key) == value for key, value in query.labels.items())
        ]

    def create(self, params: Any) -> _FakeSandbox:
        self.create_params.append(params)
        sandbox = _FakeSandbox(
            id=f"sandbox-{len(self.sandboxes) + 1}",
            labels=dict(params.labels),
            env=dict(params.env_vars or {}),
        )
        self.sandboxes.append(sandbox)
        return sandbox

    def start(self, sandbox: _FakeSandbox) -> None:
        self.started.append(sandbox)
        sandbox.state = "started"

    def get(self, sandbox_id: str) -> _FakeSandbox:
        self.gotten.append(sandbox_id)
        return next(sandbox for sandbox in self.sandboxes if sandbox.id == sandbox_id)

    def delete(self, sandbox: _FakeSandbox) -> None:
        self.deleted.append(sandbox)


def _lifecycle(client: _FakeClient, tokens: list[str] | None = None) -> DaytonaLifecycle:
    token_reads = tokens if tokens is not None else []

    def read_token() -> str:
        token_reads.append("read")
        return "ghp_clone_secret"

    return DaytonaLifecycle(client_factory=lambda _request: client, token_reader=read_token)


def test_ensure_reuses_and_starts_single_stopped_match_without_reading_token() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(request))
    client = _FakeClient([sandbox])
    token_reads: list[str] = []

    workspace = _lifecycle(client, token_reads).ensure(request)

    assert workspace.id == "sandbox-1"
    assert workspace.state == "started"
    assert workspace.path == REMOTE_REPO_REL
    assert workspace.owner == owner_key(request)
    assert client.started == [sandbox]
    assert client.create_params == []
    assert token_reads == []


def test_ensure_rejects_multiple_matches_with_sorted_ids() -> None:
    request = _request()
    client = _FakeClient(
        [
            _FakeSandbox(id="sandbox-z", labels=labels_for(request)),
            _FakeSandbox(id="sandbox-a", labels=labels_for(request)),
        ]
    )

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(request)

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "ensure"
    assert str(raised.value) == "multiple matching sandboxes: sandbox-a, sandbox-z"


def test_ensure_creates_coder_without_auto_delete_and_clones_secret_safely() -> None:
    request = _request()
    client = _FakeClient()

    workspace = _lifecycle(client).ensure(request)

    assert workspace.id == "sandbox-1"
    params = client.create_params[0]
    assert params.name == owner_key(request)
    assert params.labels == labels_for(request)
    assert params.snapshot == "clipse-snapshot"
    assert params.auto_stop_interval == 60
    assert params.auto_delete_interval is None
    assert params.env_vars == {}
    assert client.sandboxes[0].git.clone_calls == [
        {
            "url": request.repo_url,
            "path": REMOTE_REPO_REL,
            "branch": request.base_branch,
            "username": "x-access-token",
            "password": "ghp_clone_secret",
        }
    ]
    assert "ghp_clone_secret" not in repr(client.sandboxes[0].env)
    assert "ghp_clone_secret" not in repr(client.sandboxes[0].git.config)


def test_ensure_creates_reviewer_with_auto_delete_and_run_scoped_labels() -> None:
    request = _request(role="reviewer")
    client = _FakeClient()

    _lifecycle(client).ensure(request)

    params = client.create_params[0]
    assert params.labels == labels_for(request)
    assert params.auto_stop_interval == 60
    assert params.auto_delete_interval == 45
    assert client.sandboxes[0].git.clone_calls[0]["branch"] == request.branch


def test_list_returns_only_matching_typed_workspaces() -> None:
    request = _request(action="list")
    matching = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(request))
    client = _FakeClient(
        [
            matching,
            _FakeSandbox(id="sandbox-other", labels={"created-by": "someone-else"}),
        ]
    )

    workspaces = _lifecycle(client).list(request)

    assert [workspace.id for workspace in workspaces] == ["sandbox-1"]
    assert workspaces[0].state == "stopped"
    assert client.list_queries[0].labels == labels_for(request)


def test_delete_uses_explicit_sandbox_id() -> None:
    sandbox = _FakeSandbox(id="sandbox-1")
    client = _FakeClient([sandbox])
    request = _request(action="delete", sandbox_id="sandbox-1")

    workspace = _lifecycle(client).delete(request)

    assert workspace.id == "sandbox-1"
    assert workspace.state == "destroyed"
    assert client.gotten == ["sandbox-1"]
    assert client.deleted == [sandbox]


def test_delete_requires_sandbox_id() -> None:
    with pytest.raises(BackendActionError) as raised:
        _lifecycle(_FakeClient()).delete(_request(action="delete"))

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "delete"
