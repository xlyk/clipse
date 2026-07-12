"""Fake-backed tests for the Daytona sandbox lifecycle."""

from __future__ import annotations

import asyncio
import os
import shlex
from dataclasses import dataclass, field
from typing import Any

import pytest
from daytona import DaytonaConflictError, DaytonaConnectionError, DaytonaNotFoundError, DaytonaValidationError
from deepagents.backends.protocol import ExecuteResponse

from clipse_agent.backends.contracts import BackendActionRequest, BackendWorkspace
from clipse_agent.backends.daytona import (
    REMOTE_REPO_ABS,
    REMOTE_REPO_REL,
    BackendActionError,
    DaytonaLifecycle,
    DaytonaSession,
    RepositoryScopedDaytonaSandbox,
    labels_for,
    owner_key,
)
from clipse_agent.backends.session import CommandResult


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
    current_branch: str = "main"
    created_branches: list[tuple[str, str]] = field(default_factory=list)
    checked_out_branches: list[tuple[str, str]] = field(default_factory=list)
    local_branches: set[str] = field(default_factory=lambda: {"main"})
    create_branch_error_after: BaseException | None = None
    checkout_branch_error_after: BaseException | None = None
    branches_error_on_calls: set[int] = field(default_factory=set)
    branches_calls: int = 0

    def clone(self, **kwargs: Any) -> None:
        if self.events is not None:
            self.events.append("clone")
        self.clone_calls.append(kwargs)
        if self.raises is not None:
            raise self.raises
        self.current_branch = kwargs["branch"]
        self.local_branches.add(kwargs["branch"])

    def status(self, path: str) -> Any:
        assert path == REMOTE_REPO_ABS
        return type("Status", (), {"current_branch": self.current_branch})()

    def create_branch(self, path: str, name: str) -> None:
        self.created_branches.append((path, name))
        self.local_branches.add(name)
        if self.create_branch_error_after is not None:
            raise self.create_branch_error_after

    def checkout_branch(self, path: str, branch: str) -> None:
        self.checked_out_branches.append((path, branch))
        self.current_branch = branch
        if self.checkout_branch_error_after is not None:
            raise self.checkout_branch_error_after

    def branches(self, path: str) -> Any:
        assert path == REMOTE_REPO_ABS
        self.branches_calls += 1
        if self.branches_calls in self.branches_error_on_calls:
            raise RuntimeError("lost branch-state response canary")
        return type("Branches", (), {"branches": sorted(self.local_branches)})()


@dataclass
class _FakeFs:
    repo_present: bool = True
    raises: BaseException | None = None

    def get_file_info(self, path: str) -> object:
        assert path == f"{REMOTE_REPO_REL}/.git"
        if self.raises is not None:
            raise self.raises
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
        create_error: BaseException | None = None,
        delete_error: BaseException | None = None,
        get_error: BaseException | None = None,
        list_error: BaseException | None = None,
        start_error: BaseException | None = None,
        create_branch_error_after: BaseException | None = None,
        branches_error_on_calls: set[int] | None = None,
        events: list[str] | None = None,
    ) -> None:
        self.sandboxes = list(sandboxes or [])
        self.clone_error = clone_error
        self.create_error = create_error
        self.delete_error = delete_error
        self.get_error = get_error
        self.list_error = list_error
        self.start_error = start_error
        self.create_branch_error_after = create_branch_error_after
        self.branches_error_on_calls = set(branches_error_on_calls or set())
        self.events = events
        self.list_queries: list[Any] = []
        self.create_params: list[Any] = []
        self.started: list[_FakeSandbox] = []
        self.deleted: list[_FakeSandbox] = []
        self.gotten: list[str] = []

    def list(self, query: Any) -> list[_FakeSandbox]:
        self.list_queries.append(query)
        if self.list_error is not None:
            raise self.list_error
        return [
            sandbox
            for sandbox in self.sandboxes
            if all(sandbox.labels.get(key) == value for key, value in query.labels.items())
        ]

    def create(self, params: Any) -> _FakeSandbox:
        if self.events is not None:
            self.events.append("create")
        if self.create_error is not None:
            raise self.create_error
        self.create_params.append(params)
        sandbox = _FakeSandbox(
            id=f"sandbox-{len(self.sandboxes) + 1}",
            labels=dict(params.labels),
            env=dict(params.env_vars or {}),
            git=_FakeGit(
                raises=self.clone_error,
                events=self.events,
                create_branch_error_after=self.create_branch_error_after,
                branches_error_on_calls=set(self.branches_error_on_calls),
            ),
        )
        self.sandboxes.append(sandbox)
        return sandbox

    def start(self, sandbox: _FakeSandbox) -> None:
        if self.start_error is not None:
            raise self.start_error
        self.started.append(sandbox)
        sandbox.state = "started"

    def get(self, sandbox_id: str) -> _FakeSandbox:
        self.gotten.append(sandbox_id)
        if self.get_error is not None:
            raise self.get_error
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
    preflights: list[str] | None = None,
    token_error: BaseException | None = None,
    remote_branch_exists: bool = False,
    branch_probe_error: BaseException | None = None,
) -> DaytonaLifecycle:
    token_reads = tokens if tokens is not None else []

    def read_token() -> str:
        token_reads.append("read")
        if client.events is not None:
            client.events.append("token")
        if token_error is not None:
            raise token_error
        return "ghp_clone_secret"

    def preflight() -> None:
        if preflights is not None:
            preflights.append("status")

    def branch_probe(_repo_slug: str, _branch: str) -> bool:
        if branch_probe_error is not None:
            raise branch_probe_error
        return remote_branch_exists

    return DaytonaLifecycle(
        client_factory=lambda _request: client,
        token_reader=read_token,
        auth_preflight=preflight,
        branch_exists=branch_probe,
    )


def test_ensure_reuses_and_starts_single_stopped_match_without_reading_token() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(request))
    sandbox.git.current_branch = request.branch
    client = _FakeClient([sandbox])
    token_reads: list[str] = []

    workspace = _lifecycle(client, token_reads).ensure(request)

    assert workspace.external_id == "sandbox-1"
    assert workspace.state == "active"
    assert workspace.workspace_path == REMOTE_REPO_ABS
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
    assert workspace.state == "active"
    assert workspace.workspace_path == REMOTE_REPO_ABS
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
    assert client.sandboxes[0].git.created_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert client.sandboxes[0].git.checked_out_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert client.sandboxes[0].git.current_branch == request.branch
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
    assert params.env_vars == {}
    assert client.sandboxes[0].git.clone_calls[0]["branch"] == request.branch
    assert "ghp_clone_secret" not in repr(client.sandboxes[0].env)
    assert "ghp_clone_secret" not in repr(client.sandboxes[0].git.config)


def test_ensure_coder_restores_existing_remote_feature_branch() -> None:
    request = _request()
    client = _FakeClient()

    _lifecycle(client, remote_branch_exists=True).ensure(request)

    assert client.sandboxes[0].git.clone_calls[0]["branch"] == request.branch
    assert client.sandboxes[0].git.created_branches == []
    assert client.sandboxes[0].git.current_branch == request.branch


def test_ensure_branch_lookup_failure_creates_no_sandbox() -> None:
    client = _FakeClient()

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client, branch_probe_error=TimeoutError("network canary")).ensure(_request())

    assert raised.value.kind == "transient"
    assert client.create_params == []
    assert client.sandboxes == []


def test_ensure_coder_repairs_reused_sandbox_to_requested_branch() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", labels=labels_for(request))
    sandbox.git.current_branch = request.base_branch
    client = _FakeClient([sandbox])

    workspace = _lifecycle(client, remote_branch_exists=False).ensure(request)

    assert workspace.external_id == sandbox.id
    assert sandbox.git.created_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert sandbox.git.checked_out_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert sandbox.git.current_branch == request.branch


def test_ensure_coder_recovers_lost_create_response_then_retry_checks_out_existing_branch() -> None:
    request = _request()
    client = _FakeClient(
        create_branch_error_after=RuntimeError("lost create response canary"),
        delete_error=RuntimeError("lost cleanup response canary"),
        branches_error_on_calls={2},
    )

    with pytest.raises(BackendActionError) as first:
        _lifecycle(client, remote_branch_exists=False).ensure(request)
    assert first.value.kind == "transient"
    assert first.value.operation == "clone_cleanup"
    sandbox = client.sandboxes[0]
    assert request.branch in sandbox.git.local_branches

    client.delete_error = None
    client.create_branch_error_after = None
    sandbox.git.create_branch_error_after = None
    workspace = _lifecycle(client, remote_branch_exists=False).ensure(request)

    assert workspace.external_id == sandbox.id
    assert request.branch in sandbox.git.local_branches
    assert sandbox.git.current_branch == request.branch
    assert sandbox.git.checked_out_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert sandbox.git.created_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert client.deleted == [sandbox]


def test_ensure_coder_checks_out_existing_local_feature_instead_of_recreating() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", labels=labels_for(request))
    sandbox.git.local_branches.add(request.branch)
    client = _FakeClient([sandbox])

    _lifecycle(client, remote_branch_exists=False).ensure(request)

    assert sandbox.git.created_branches == []
    assert sandbox.git.checked_out_branches == [(REMOTE_REPO_ABS, request.branch)]
    assert sandbox.git.current_branch == request.branch


def test_ensure_coder_accepts_ambiguous_checkout_when_branch_is_current() -> None:
    request = _request()
    sandbox = _FakeSandbox(id="sandbox-1", labels=labels_for(request))
    sandbox.git.local_branches.add(request.branch)
    sandbox.git.checkout_branch_error_after = RuntimeError("lost checkout response canary")
    client = _FakeClient([sandbox])

    workspace = _lifecycle(client, remote_branch_exists=False).ensure(request)

    assert workspace.external_id == sandbox.id
    assert sandbox.git.current_branch == request.branch


def test_ensure_coder_recreates_wrong_branch_from_pushed_remote_feature() -> None:
    request = _request()
    stale = _FakeSandbox(id="sandbox-stale", labels=labels_for(request))
    stale.git.current_branch = request.base_branch
    client = _FakeClient([stale])

    workspace = _lifecycle(client, remote_branch_exists=True).ensure(request)

    assert workspace.external_id == "sandbox-2"
    assert client.deleted == [stale]
    assert client.sandboxes[-1].git.clone_calls[0]["branch"] == request.branch
    assert client.sandboxes[-1].git.current_branch == request.branch


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
    assert workspaces[0].workspace_path == REMOTE_REPO_ABS
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
    assert workspace.state == "deleted"
    assert workspace.workspace_path == REMOTE_REPO_ABS
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


def test_ensure_classifies_temporary_clone_and_cleanup_failure_as_transient() -> None:
    client = _FakeClient(
        clone_error=DaytonaConnectionError("clone failed with ghp_secret"),
        delete_error=DaytonaConnectionError("delete failed with ghp_secret"),
    )

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "transient"
    assert raised.value.operation == "clone_cleanup"
    assert str(raised.value) == "clone failed and cleanup failed for sandbox sandbox-1 (DaytonaConnectionError)"
    assert "ghp_secret" not in str(raised.value)


def test_ensure_classifies_validation_clone_cleanup_failure_as_needs_input() -> None:
    client = _FakeClient(
        clone_error=DaytonaValidationError("invalid clone"),
        delete_error=RuntimeError("delete lost response"),
    )

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "clone_cleanup"
    assert [sandbox.id for sandbox in client.sandboxes] == ["sandbox-1"]


def test_ensure_classifies_validation_cleanup_after_transient_clone_as_needs_input() -> None:
    client = _FakeClient(
        clone_error=DaytonaConnectionError("temporary clone"),
        delete_error=DaytonaValidationError("invalid delete configuration"),
    )

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "clone_cleanup"


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
    preflights: list[str] = []
    request = BackendActionRequest(action="list", provider="daytona", repo_slug="xlyk/clipse")

    workspaces = _lifecycle(client, token_reads, preflights=preflights).list(request)

    assert preflights == ["status"]
    assert token_reads == []
    assert workspaces == [
        BackendWorkspace(
            external_id="sandbox-1",
            state="active",
            workspace_path=REMOTE_REPO_ABS,
            owner_key="daytona:xlyk/clipse:reviewer:issue-1:run-1",
        )
    ]


def test_ensure_maps_missing_snapshot_to_needs_input() -> None:
    client = _FakeClient(create_error=DaytonaNotFoundError("snapshot missing"))

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "daytona_config"


def test_ensure_maps_invalid_target_during_client_setup_to_needs_input() -> None:
    def invalid_target(_request: BackendActionRequest) -> _FakeClient:
        raise DaytonaNotFoundError("target missing")

    lifecycle = DaytonaLifecycle(client_factory=invalid_target, token_reader=lambda: "token")

    with pytest.raises(BackendActionError) as raised:
        lifecycle.ensure(_request())

    assert raised.value.kind == "needs_input"
    assert raised.value.operation == "daytona_config"


def test_ensure_maps_sandbox_vanishing_during_start_to_transient() -> None:
    request = _request()
    stopped = _FakeSandbox(id="sandbox-1", state="stopped", labels=labels_for(request))
    client = _FakeClient([stopped], start_error=DaytonaNotFoundError("sandbox gone"))

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(request)

    assert raised.value.kind == "transient"
    assert raised.value.operation == "ensure"


def test_ensure_maps_sandbox_vanishing_during_match_to_transient() -> None:
    client = _FakeClient(list_error=DaytonaNotFoundError("sandbox gone"))

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(_request())

    assert raised.value.kind == "transient"
    assert raised.value.operation == "ensure"


def test_ensure_maps_sandbox_vanishing_during_attach_to_transient() -> None:
    request = _request()
    vanished = _FakeSandbox(
        id="sandbox-1",
        labels=labels_for(request),
        fs=_FakeFs(raises=DaytonaNotFoundError("sandbox gone")),
    )
    client = _FakeClient([vanished], get_error=DaytonaNotFoundError("sandbox gone"))

    with pytest.raises(BackendActionError) as raised:
        _lifecycle(client).ensure(request)

    assert raised.value.kind == "transient"
    assert raised.value.operation == "ensure"


def test_delete_already_missing_sandbox_is_successfully_deleted() -> None:
    client = _FakeClient(get_error=DaytonaNotFoundError("sandbox gone"))
    request = _request(action="delete", sandbox_id="sandbox-1")

    workspace = _lifecycle(client).delete(request)

    assert workspace.external_id == "sandbox-1"
    assert workspace.state == "deleted"
    assert client.deleted == []


def test_delete_sandbox_vanishing_after_lookup_is_successfully_deleted() -> None:
    sandbox = _FakeSandbox(id="sandbox-1")
    client = _FakeClient([sandbox], delete_error=DaytonaNotFoundError("sandbox gone"))
    request = _request(action="delete", sandbox_id="sandbox-1")

    workspace = _lifecycle(client).delete(request)

    assert workspace.external_id == "sandbox-1"
    assert workspace.state == "deleted"
    assert client.deleted == [sandbox]


@dataclass
class _FakeExecuteResponse:
    output: str
    exit_code: int | None


@dataclass
class _FakeDaytonaBackend:
    calls: list[str] = field(default_factory=list)
    output: str = "remote output"
    exit_code: int | None = 0

    def execute(self, command: str, *, timeout: int | None = None) -> _FakeExecuteResponse:
        self.calls.append(command)
        return _FakeExecuteResponse(output=self.output, exit_code=self.exit_code)


@dataclass
class _FakeSessionGit:
    pull_calls: list[dict[str, Any]] = field(default_factory=list)
    pull_error: BaseException | None = None
    add_calls: list[dict[str, Any]] = field(default_factory=list)
    commit_calls: list[dict[str, Any]] = field(default_factory=list)
    push_calls: list[dict[str, Any]] = field(default_factory=list)
    transcript: list[dict[str, Any]] = field(default_factory=list)

    def pull(self, path: str, /, **kwargs: Any) -> None:
        self.pull_calls.append({"path": path, **kwargs})
        self.transcript.append({"event": "git.pull", "branch": kwargs["branch"]})
        if self.pull_error is not None:
            raise self.pull_error

    def add(self, path: str, files: list[str]) -> None:
        self.add_calls.append({"path": path, "files": files})
        self.transcript.append({"event": "git.add", "files": files})

    def commit(self, **kwargs: Any) -> object:
        self.commit_calls.append(kwargs)
        self.transcript.append({"event": "git.commit", "message": kwargs["message"]})
        return object()

    def push(self, **kwargs: Any) -> None:
        self.push_calls.append(kwargs)
        self.transcript.append({"event": "git.push", "branch": kwargs["branch"]})


@dataclass
class _FakeSessionSandbox:
    git: _FakeSessionGit = field(default_factory=_FakeSessionGit)


def test_daytona_session_run_executes_through_daytona_backend() -> None:
    backend = _FakeDaytonaBackend()
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        backend,
        _FakeSessionSandbox(),
        token_reader=lambda: "unused",
        host_runner=lambda _argv: "unused",
    )

    result = session.run(["python", "-c", "print('hello world')"])

    assert backend.calls == [shlex.join(["python", "-c", "print('hello world')"])]
    assert result == CommandResult(returncode=0, stdout="remote output", stderr="")
    assert session.provider == "daytona"
    assert session.sandbox is backend
    assert session.sandbox_type == "daytona"


def test_daytona_session_run_sanitizes_backend_exceptions() -> None:
    canary = "provider-body-ghp_attach_canary"

    class RaisingBackend:
        def execute(self, _command: str) -> _FakeExecuteResponse:
            raise RuntimeError(canary)

    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        RaisingBackend(),
        _FakeSessionSandbox(),
        token_reader=lambda: "unused",
        host_runner=lambda _argv: "unused",
    )

    result = session.run(["pwd"])

    assert result == CommandResult(1, stderr="Daytona command failed")
    assert canary not in repr(result)


def test_daytona_session_git_credentials_are_read_just_in_time() -> None:
    token_reads: list[str] = []
    tokens = iter(["pull-token", "push-token"])

    def read_token() -> str:
        token_reads.append("read")
        return next(tokens)

    sdk_sandbox = _FakeSessionSandbox()
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        _FakeDaytonaBackend(),
        sdk_sandbox,
        token_reader=read_token,
        host_runner=lambda _argv: "unused",
    )

    assert token_reads == []
    assert session.sync_base("main") == CommandResult(0)
    assert token_reads == ["read"]
    assert sdk_sandbox.git.pull_calls == [
        {
            "path": REMOTE_REPO_ABS,
            "username": "x-access-token",
            "password": "pull-token",
            "branch": "main",
            "remote": "origin",
        }
    ]

    assert session.push("feat/CLI-1") == CommandResult(0)
    assert token_reads == ["read", "read"]
    assert sdk_sandbox.git.push_calls == [
        {
            "path": REMOTE_REPO_ABS,
            "username": "x-access-token",
            "password": "push-token",
            "branch": "feat/CLI-1",
            "remote": "origin",
            "set_upstream": True,
        }
    ]


def test_daytona_session_sync_base_materializes_sdk_conflict_with_remote_merge() -> None:
    canary = "provider-body-ghp_conflict_canary"
    backend = _FakeDaytonaBackend(output="README.md: needs merge", exit_code=1)
    sdk_sandbox = _FakeSessionSandbox(
        git=_FakeSessionGit(pull_error=DaytonaConflictError(canary, status_code=409))
    )
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        backend,
        sdk_sandbox,
        token_reader=lambda: "pull-token",
        host_runner=lambda _argv: "unused",
    )

    result = session.sync_base("main")

    assert sdk_sandbox.git.pull_calls == [
        {
            "path": REMOTE_REPO_ABS,
            "username": "x-access-token",
            "password": "pull-token",
            "branch": "main",
            "remote": "origin",
        }
    ]
    assert backend.calls == [
        shlex.join(
            [
                "git",
                "-c",
                "user.name=clipse",
                "-c",
                "user.email=clipse@users.noreply.github.com",
                "merge",
                "--no-edit",
                "origin/main",
            ]
        )
    ]
    assert result == CommandResult(1, stdout="README.md: needs merge")
    assert canary not in repr(result)


def test_daytona_authenticated_git_calls_leave_no_credential_surface(monkeypatch) -> None:
    token = "ghp_daytona_session_canary"
    monkeypatch.delenv("GH_TOKEN", raising=False)
    monkeypatch.delenv("GITHUB_TOKEN", raising=False)
    backend = _FakeDaytonaBackend()
    sdk_sandbox = _FakeSessionSandbox()
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        backend,
        sdk_sandbox,
        token_reader=lambda: token,
        host_runner=lambda _argv: "unused",
    )

    for result in (session.sync_base("main"), session.push("feat/CLI-1")):
        config = session.run(["git", "config", "--get-regexp", r"^remote\.origin\."])
        assert token not in repr(dict(os.environ))
        assert token not in config.stdout
        assert token not in repr(result)
        assert token not in repr(sdk_sandbox.git.transcript)


def test_daytona_session_commit_uses_sdk_git_and_github_stays_on_host() -> None:
    host_calls: list[list[str]] = []
    sdk_sandbox = _FakeSessionSandbox()
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        _FakeDaytonaBackend(),
        sdk_sandbox,
        token_reader=lambda: "unused",
        host_runner=lambda argv: host_calls.append(argv) or "https://github.com/xlyk/clipse/pull/1",
    )

    assert session.commit("feat: remote change") == CommandResult(0)
    assert sdk_sandbox.git.add_calls == [{"path": REMOTE_REPO_ABS, "files": ["."]}]
    assert sdk_sandbox.git.commit_calls == [
        {
            "path": REMOTE_REPO_ABS,
            "message": "feat: remote change",
            "author": "clipse",
            "email": "clipse@users.noreply.github.com",
        }
    ]

    result = session.github(["pr", "view", "feat/CLI-1", "--json", "url"])

    assert result == CommandResult(
        0,
        "https://github.com/xlyk/clipse/pull/1",
        "",
    )
    assert host_calls == [
        [
            "gh",
            "pr",
            "view",
            "feat/CLI-1",
            "--json",
            "url",
            "--repo",
            "xlyk/clipse",
        ]
    ]


def test_daytona_session_github_replaces_caller_scope_and_expands_api_repository() -> None:
    host_calls: list[list[str]] = []
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        _FakeDaytonaBackend(),
        _FakeSessionSandbox(),
        token_reader=lambda: "unused",
        host_runner=lambda argv: host_calls.append(argv) or "",
    )

    assert session.github(["pr", "diff", "feat/CLI-1", "--repo", "other/repo"]) == CommandResult(0)
    assert session.github(
        ["api", "repos/{owner}/{repo}/pulls/7/comments", "-f", "body=keep {owner}/{repo} literal"]
    ) == CommandResult(0)

    assert host_calls == [
        ["gh", "pr", "diff", "feat/CLI-1", "--repo", "xlyk/clipse"],
        [
            "gh",
            "api",
            "repos/xlyk/clipse/pulls/7/comments",
            "-f",
            "body=keep {owner}/{repo} literal",
        ],
    ]


def test_daytona_session_commit_merge_preserves_message_and_supplies_author() -> None:
    backend = _FakeDaytonaBackend()
    session = DaytonaSession(
        REMOTE_REPO_ABS,
        "xlyk/clipse",
        backend,
        _FakeSessionSandbox(),
        token_reader=lambda: "unused",
        host_runner=lambda _argv: "unused",
    )

    assert session.commit_merge() == CommandResult(0, stdout="remote output")
    assert backend.calls == [
        "git -c user.name=clipse -c user.email=clipse@users.noreply.github.com commit --no-edit"
    ]


class _RecordingBackend:
    """Pinned SandboxBackendProtocol surface used to verify full delegation."""

    id = "sandbox-1"

    def __init__(self) -> None:
        self.calls: list[tuple[Any, ...]] = []

    def _record(self, *call: Any) -> tuple[Any, ...]:
        self.calls.append(call)
        return call

    def execute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        self.calls.append(("execute", command, timeout))
        output = REMOTE_REPO_ABS if command.endswith("pwd") else f"git-dir:{REMOTE_REPO_ABS}/.git"
        return ExecuteResponse(output=output, exit_code=0, truncated=False)

    async def aexecute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        self.calls.append(("aexecute", command, timeout))
        return ExecuteResponse(output=REMOTE_REPO_ABS, exit_code=0, truncated=False)

    def ls(self, path: str) -> tuple[Any, ...]:
        return self._record("ls", path)

    async def als(self, path: str) -> tuple[Any, ...]:
        return self._record("als", path)

    def read(self, file_path: str, offset: int = 0, limit: int = 2000) -> tuple[Any, ...]:
        return self._record("read", file_path, offset, limit)

    async def aread(self, file_path: str, offset: int = 0, limit: int = 2000) -> tuple[Any, ...]:
        return self._record("aread", file_path, offset, limit)

    def grep(self, pattern: str, path: str | None = None, glob: str | None = None) -> tuple[Any, ...]:
        return self._record("grep", pattern, path, glob)

    async def agrep(
        self, pattern: str, path: str | None = None, glob: str | None = None
    ) -> tuple[Any, ...]:
        return self._record("agrep", pattern, path, glob)

    def glob(self, pattern: str, path: str | None = None) -> tuple[Any, ...]:
        return self._record("glob", pattern, path)

    async def aglob(self, pattern: str, path: str | None = None) -> tuple[Any, ...]:
        return self._record("aglob", pattern, path)

    def write(self, file_path: str, content: str) -> tuple[Any, ...]:
        return self._record("write", file_path, content)

    async def awrite(self, file_path: str, content: str) -> tuple[Any, ...]:
        return self._record("awrite", file_path, content)

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> tuple[Any, ...]:
        return self._record("edit", file_path, old_string, new_string, replace_all)

    async def aedit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> tuple[Any, ...]:
        return self._record("aedit", file_path, old_string, new_string, replace_all)

    def upload_files(self, files: list[tuple[str, bytes]]) -> tuple[Any, ...]:
        return self._record("upload_files", files)

    async def aupload_files(self, files: list[tuple[str, bytes]]) -> tuple[Any, ...]:
        return self._record("aupload_files", files)

    def download_files(self, paths: list[str]) -> tuple[Any, ...]:
        return self._record("download_files", paths)

    async def adownload_files(self, paths: list[str]) -> tuple[Any, ...]:
        return self._record("adownload_files", paths)


def test_repository_scoped_backend_starts_shell_in_remote_repo() -> None:
    raw = _RecordingBackend()
    scoped = RepositoryScopedDaytonaSandbox(raw, REMOTE_REPO_ABS)

    pwd = scoped.execute("pwd")
    git = scoped.execute("git rev-parse --git-dir", timeout=30)

    assert pwd.output == REMOTE_REPO_ABS
    assert git.output == f"git-dir:{REMOTE_REPO_ABS}/.git"
    assert raw.calls == [
        ("execute", f"cd {shlex.quote(REMOTE_REPO_ABS)} && pwd", None),
        (
            "execute",
            f"cd {shlex.quote(REMOTE_REPO_ABS)} && git rev-parse --git-dir",
            30,
        ),
    ]


def test_repository_scoped_backend_resolves_all_sync_file_methods() -> None:
    raw = _RecordingBackend()
    scoped = RepositoryScopedDaytonaSandbox(raw, REMOTE_REPO_ABS)

    assert scoped.ls("src") == ("ls", f"{REMOTE_REPO_ABS}/src")
    assert scoped.read("README.md", 2, 10) == ("read", f"{REMOTE_REPO_ABS}/README.md", 2, 10)
    assert scoped.grep("needle") == ("grep", "needle", REMOTE_REPO_ABS, None)
    assert scoped.glob("**/*.py") == ("glob", "**/*.py", REMOTE_REPO_ABS)
    assert scoped.write("notes/out.txt", "body") == (
        "write",
        f"{REMOTE_REPO_ABS}/notes/out.txt",
        "body",
    )
    assert scoped.edit("README.md", "old", "new", True) == (
        "edit",
        f"{REMOTE_REPO_ABS}/README.md",
        "old",
        "new",
        True,
    )
    assert scoped.upload_files([("new.bin", b"x"), ("/tmp/absolute.bin", b"y")]) == (
        "upload_files",
        [(f"{REMOTE_REPO_ABS}/new.bin", b"x"), ("/tmp/absolute.bin", b"y")],
    )
    assert scoped.download_files(["README.md", "/tmp/absolute.bin"]) == (
        "download_files",
        [f"{REMOTE_REPO_ABS}/README.md", "/tmp/absolute.bin"],
    )
    assert scoped.read("/tmp/absolute.txt") == ("read", "/tmp/absolute.txt", 0, 2000)


async def _exercise_async_backend(scoped: RepositoryScopedDaytonaSandbox) -> None:
    command = f"cd {shlex.quote(REMOTE_REPO_ABS)} && pwd"
    assert (await scoped.aexecute("pwd", timeout=5)).output == REMOTE_REPO_ABS
    assert await scoped.als("src") == ("als", f"{REMOTE_REPO_ABS}/src")
    assert await scoped.aread("README.md", 1, 4) == (
        "aread",
        f"{REMOTE_REPO_ABS}/README.md",
        1,
        4,
    )
    assert await scoped.agrep("needle", "src", "*.py") == (
        "agrep",
        "needle",
        f"{REMOTE_REPO_ABS}/src",
        "*.py",
    )
    assert await scoped.aglob("*.md") == ("aglob", "*.md", REMOTE_REPO_ABS)
    assert await scoped.awrite("notes/out.txt", "body") == (
        "awrite",
        f"{REMOTE_REPO_ABS}/notes/out.txt",
        "body",
    )
    assert await scoped.aedit("README.md", "old", "new") == (
        "aedit",
        f"{REMOTE_REPO_ABS}/README.md",
        "old",
        "new",
        False,
    )
    assert await scoped.aupload_files([("new.bin", b"x")]) == (
        "aupload_files",
        [(f"{REMOTE_REPO_ABS}/new.bin", b"x")],
    )
    assert await scoped.adownload_files(["README.md"]) == (
        "adownload_files",
        [f"{REMOTE_REPO_ABS}/README.md"],
    )
    assert scoped.backend.calls[0] == ("aexecute", command, 5)


def test_repository_scoped_backend_resolves_all_async_methods() -> None:
    scoped = RepositoryScopedDaytonaSandbox(_RecordingBackend(), REMOTE_REPO_ABS)
    asyncio.run(_exercise_async_backend(scoped))


def test_repository_scoped_backend_rejects_relative_escape() -> None:
    scoped = RepositoryScopedDaytonaSandbox(_RecordingBackend(), REMOTE_REPO_ABS)

    with pytest.raises(ValueError, match="escapes repository root"):
        scoped.read("../outside.txt")
