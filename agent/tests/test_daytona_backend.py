"""Fake-backed tests for the Daytona sandbox lifecycle."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import pytest

from clipse_agent.backends.contracts import BackendActionRequest, BackendWorkspace
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
    raises: BaseException | None = None
    events: list[str] | None = None

    def clone(self, **kwargs: Any) -> None:
        if self.events is not None:
            self.events.append("clone")
        self.clone_calls.append(kwargs)
        if self.raises is not None:
            raise self.raises


@dataclass
class _FakeFs:
    repo_present: bool = True

    def get_file_info(self, path: str) -> object:
        assert path == f"{REMOTE_REPO_REL}/.git"
        if not self.repo_present:
            raise FileNotFoundError(path)
        return object()


@dataclass
class _FakeSandbox:
    id: str
    state: str = "started"
    labels: dict[str, str] = field(default_factory=dict)
    env: dict[str, str] = field(default_factory=dict)
    git: _FakeGit = field(default_factory=_FakeGit)
    fs: _FakeFs = field(default_factory=_FakeFs)


class _FakeClient:
    def __init__(
        self,
        sandboxes: list[_FakeSandbox] | None = None,
        *,
        clone_error: BaseException | None = None,
        delete_error: BaseException | None = None,
        events: list[str] | None = None,
    ) -> None:
        self.sandboxes = list(sandboxes or [])
        self.clone_error = clone_error
        self.delete_error = delete_error
        self.events = events
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
        if self.events is not None:
            self.events.append("create")
        self.create_params.append(params)
        sandbox = _FakeSandbox(
            id=f"sandbox-{len(self.sandboxes) + 1}",
            labels=dict(params.labels),
            env=dict(params.env_vars or {}),
            git=_FakeGit(raises=self.clone_error, events=self.events),
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
        if self.events is not None:
            self.events.append("delete")
        self.deleted.append(sandbox)
        if self.delete_error is not None:
            raise self.delete_error


def _lifecycle(
    client: _FakeClient,
    tokens: list[str] | None = None,
    *,
    token_error: BaseException | None = None,
) -> DaytonaLifecycle:
    token_reads = tokens if tokens is not None else []

    def read_token() -> str:
        token_reads.append("read")
        if client.events is not None:
            client.events.append("token")
        if token_error is not None:
            raise token_error
        return "ghp_clone_secret"

    return DaytonaLifecycle(client_factory=lambda _request: client, token_reader=read_token)


def test_ensure_reuses_and_starts_single_stopped_match_without_reading_token() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(request))
    client = _FakeClient([sandbox])
    token_reads: list[str] = []

    workspace = _lifecycle(client, token_reads).ensure(request)

    assert workspace.external_id == "sandbox-1"
    assert workspace.state == "started"
    assert workspace.workspace_path == REMOTE_REPO_REL
    assert workspace.owner_key == owner_key(request)
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

    assert workspace.external_id == "sandbox-1"
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
    scoped = _request()
    request = BackendActionRequest(action="list", provider="daytona", repo_slug="xlyk/clipse")
    matching = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(scoped))
    client = _FakeClient(
        [
            matching,
            _FakeSandbox(id="sandbox-other", labels={"created-by": "someone-else"}),
        ]
    )

    workspaces = _lifecycle(client).list(request)

    assert [workspace.external_id for workspace in workspaces] == ["sandbox-1"]
    assert workspaces[0].owner_key == owner_key(scoped)
    assert workspaces[0].state == "stopped"
    assert client.list_queries[0].labels == {
        "created-by": "clipse",
        "repo": labels_for(scoped)["repo"],
    }


def test_delete_uses_explicit_sandbox_id() -> None:
    sandbox = _FakeSandbox(id="sandbox-1")
    client = _FakeClient([sandbox])
    request = _request(action="delete", sandbox_id="sandbox-1")

    workspace = _lifecycle(client).delete(request)

    assert workspace.external_id == "sandbox-1"
    assert workspace.state == "destroyed"
    assert client.gotten == ["sandbox-1"]
    assert client.deleted == [sandbox]


def test_delete_requires_sandbox_id() -> None:
    with pytest.raises(ValueError):
        _request(action="delete")


def test_ensure_authenticates_github_before_creating() -> None:
    events: list[str] = []
    client = _FakeClient(events=events)

    with pytest.raises(RuntimeError, match="authenticate first"):
        _lifecycle(client, token_error=RuntimeError("authenticate first")).ensure(_request())

    assert events == ["token"]
    assert client.create_params == []


def test_ensure_deletes_new_sandbox_when_clone_fails() -> None:
    client = _FakeClient(clone_error=RuntimeError("clone failed"))

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.operation == "clone"
    assert [sandbox.id for sandbox in client.deleted] == ["sandbox-1"]


def test_ensure_surfaces_cleanup_failure_instead_of_hiding_poisoned_identity() -> None:
    client = _FakeClient(
        clone_error=RuntimeError("clone failed with ghp_secret"),
        delete_error=RuntimeError("delete failed with ghp_secret"),
    )

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "clone_cleanup"
    assert str(raised.value) == "clone failed and cleanup failed for sandbox sandbox-1 (RuntimeError)"
    assert "ghp_secret" not in str(raised.value)


def test_ensure_recreates_reused_sandbox_when_repository_is_incomplete() -> None:
    request = _request()
    stale = _FakeSandbox(id="sandbox-stale", labels=labels_for(request), fs=_FakeFs(repo_present=False))
    events: list[str] = []
    client = _FakeClient([stale], events=events)

    workspace = _lifecycle(client).ensure(request)

    assert workspace.external_id == "sandbox-2"
    assert client.deleted == [stale]
    assert events == ["token", "delete", "create", "clone"]


def test_repository_list_preflights_daytona_and_github_auth() -> None:
    scoped = _request(role="reviewer")
    sandbox = _FakeSandbox(id="sandbox-1", labels=labels_for(scoped))
    client = _FakeClient([sandbox])
    token_reads: list[str] = []
    request = BackendActionRequest(action="list", provider="daytona", repo_slug="xlyk/clipse")

    workspaces = _lifecycle(client, token_reads).list(request)

    assert token_reads == ["read"]
    assert workspaces == [
        BackendWorkspace(
            external_id="sandbox-1",
            state="started",
            workspace_path=REMOTE_REPO_REL,
            owner_key="daytona:xlyk/clipse:reviewer:issue-1:run-1",
        )
    ]
