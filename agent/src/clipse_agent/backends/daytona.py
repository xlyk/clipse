"""Fakeable Daytona sandbox lifecycle operations."""

from __future__ import annotations

import re
from collections.abc import Callable, Iterable
from typing import Any, Protocol

from daytona import CreateSandboxFromSnapshotParams, Daytona, DaytonaConfig, ListSandboxesQuery

from clipse_agent.backends.contracts import (
    BackendActionError,
    BackendActionRequest,
    BackendWorkspace,
    WorkspaceState,
)
from clipse_agent.backends.github import github_token, safe_error

REMOTE_REPO_REL = "repo"


class _Git(Protocol):
    def clone(self, **kwargs: Any) -> None: ...


class _Sandbox(Protocol):
    id: str
    state: Any
    git: _Git


class _DaytonaClient(Protocol):
    def list(self, query: ListSandboxesQuery) -> Iterable[_Sandbox]: ...

    def create(self, params: CreateSandboxFromSnapshotParams) -> _Sandbox: ...

    def start(self, sandbox: _Sandbox) -> None: ...

    def get(self, sandbox_id: str) -> _Sandbox: ...

    def delete(self, sandbox: _Sandbox) -> None: ...


ClientFactory = Callable[[BackendActionRequest], _DaytonaClient]
TokenReader = Callable[[], str]


def repo_label(repo_slug: str) -> str:
    """Return a stable Daytona-label-safe repository identity."""

    normalized = re.sub(r"[^a-z0-9_.-]+", "-", repo_slug.lower()).strip("-.")
    return normalized or "repo"


def owner_key(request: BackendActionRequest) -> str:
    """Return the stable ownership identity for a lifecycle request."""

    base = f"{request.provider}:{request.repo_slug}:{request.role}:{request.issue_id}"
    if request.role == "reviewer":
        return f"{base}:{request.run_id}"
    return base


def labels_for(request: BackendActionRequest) -> dict[str, str]:
    """Return the complete label selector used for creation and matching."""

    labels = {
        "created-by": "clipse",
        "repo": repo_label(request.repo_slug),
        "issue": request.issue_id,
        "role": request.role,
    }
    if request.role == "reviewer":
        labels["run"] = request.run_id
    return labels


def _state_value(state: Any) -> WorkspaceState:
    value = getattr(state, "value", state)
    if not isinstance(value, str):
        return "unknown"
    allowed = BackendWorkspace.model_fields["state"].annotation
    if value not in getattr(allowed, "__args__", ()):
        return "unknown"
    return value  # type: ignore[return-value]


def workspace_from(
    sandbox: _Sandbox,
    request: BackendActionRequest,
    *,
    state: WorkspaceState | None = None,
) -> BackendWorkspace:
    """Convert a Daytona SDK object into the stable public contract."""

    return BackendWorkspace(
        id=sandbox.id,
        state=state or _state_value(sandbox.state),
        path=REMOTE_REPO_REL,
        owner=owner_key(request),
    )


def _default_client_factory(request: BackendActionRequest) -> _DaytonaClient:
    return Daytona(DaytonaConfig(target=request.target))


class DaytonaLifecycle:
    """Ensure, enumerate, and delete Clipse-owned Daytona sandboxes."""

    def __init__(
        self,
        client_factory: ClientFactory = _default_client_factory,
        token_reader: TokenReader = github_token,
    ) -> None:
        self._client_factory = client_factory
        self._token_reader = token_reader
        self._client: _DaytonaClient | None = None

    def _prepare(self, request: BackendActionRequest) -> None:
        self._client = self._client_factory(request)

    def _matching(self, request: BackendActionRequest) -> list[_Sandbox]:
        assert self._client is not None
        return list(self._client.list(ListSandboxesQuery(labels=labels_for(request))))

    def _create_and_clone(self, request: BackendActionRequest) -> _Sandbox:
        assert self._client is not None
        auto_delete = request.reviewer_auto_delete_minutes if request.role == "reviewer" else None
        params = CreateSandboxFromSnapshotParams(
            name=owner_key(request),
            snapshot=request.snapshot,
            env_vars={},
            labels=labels_for(request),
            auto_stop_interval=request.auto_stop_minutes,
            auto_delete_interval=auto_delete,
        )
        sandbox = self._client.create(params)
        token = self._token_reader()
        clone_branch = request.branch if request.role == "reviewer" else request.base_branch
        try:
            sandbox.git.clone(
                url=request.repo_url,
                path=REMOTE_REPO_REL,
                branch=clone_branch,
                username="x-access-token",
                password=token,
            )
        except BackendActionError:
            raise
        except Exception as exc:
            raise BackendActionError("transient", "clone", safe_error("clone", exc)) from None
        return sandbox

    def ensure(self, request: BackendActionRequest) -> BackendWorkspace:
        self._prepare(request)
        matches = self._matching(request)
        if len(matches) > 1:
            ids = ", ".join(sorted(item.id for item in matches))
            raise BackendActionError("needs_input", "ensure", f"multiple matching sandboxes: {ids}")
        if len(matches) == 1:
            sandbox = matches[0]
            if sandbox.state != "started":
                assert self._client is not None
                self._client.start(sandbox)
        else:
            sandbox = self._create_and_clone(request)
        return workspace_from(sandbox, request)

    def delete(self, request: BackendActionRequest) -> BackendWorkspace:
        self._prepare(request)
        if not request.sandbox_id:
            raise BackendActionError("needs_input", "delete", "sandbox id is required")
        assert self._client is not None
        sandbox = self._client.get(request.sandbox_id)
        self._client.delete(sandbox)
        return workspace_from(sandbox, request, state="destroyed")

    def list(self, request: BackendActionRequest) -> list[BackendWorkspace]:
        self._prepare(request)
        return [workspace_from(sandbox, request) for sandbox in self._matching(request)]


__all__ = [
    "REMOTE_REPO_REL",
    "BackendActionError",
    "DaytonaLifecycle",
    "labels_for",
    "owner_key",
    "repo_label",
    "workspace_from",
]
