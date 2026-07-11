"""Fakeable Daytona sandbox lifecycle operations."""

from __future__ import annotations

import hashlib
from collections.abc import Callable, Iterable
from typing import Any, Protocol

from daytona import (
    CreateSandboxFromSnapshotParams,
    Daytona,
    DaytonaConfig,
    DaytonaNotFoundError,
    DaytonaValidationError,
    ListSandboxesQuery,
)

from clipse_agent.backends.contracts import (
    BackendActionError,
    BackendActionRequest,
    BackendWorkspace,
    WorkspaceState,
)
from clipse_agent.backends.github import AuthPreflight, github_auth_preflight, github_token, safe_error

REMOTE_REPO_REL = "workspace/clipse"
REMOTE_REPO_ABS = "/home/daytona/workspace/clipse"


class _Git(Protocol):
    def clone(self, **kwargs: Any) -> None: ...


class _Sandbox(Protocol):
    id: str
    state: Any
    git: _Git
    fs: Any


class _DaytonaClient(Protocol):
    def list(self, query: ListSandboxesQuery) -> Iterable[_Sandbox]: ...

    def create(self, params: CreateSandboxFromSnapshotParams) -> _Sandbox: ...

    def start(self, sandbox: _Sandbox) -> None: ...

    def get(self, sandbox_id: str) -> _Sandbox: ...

    def delete(self, sandbox: _Sandbox) -> None: ...


ClientFactory = Callable[[BackendActionRequest], _DaytonaClient]
TokenReader = Callable[[], str]


def repo_label(repo_slug: str) -> str:
    """Return a collision-resistant hash of the normalized repository identity."""

    normalized = repo_slug.strip().lower()
    if normalized.endswith(".git"):
        normalized = normalized[:-4]
    return hashlib.sha256(normalized.encode()).hexdigest()


def owner_key(request: BackendActionRequest) -> str:
    """Return the stable ownership identity for a lifecycle request."""

    assert request.role is not None
    assert request.issue_id is not None
    base = f"{request.provider}:{request.repo_slug}:{request.role}:{request.issue_id}"
    if request.role == "reviewer":
        assert request.run_id is not None
        return f"{base}:{request.run_id}"
    return base


def labels_for(request: BackendActionRequest) -> dict[str, str]:
    """Return the complete label selector used for creation and matching."""

    assert request.role is not None
    assert request.issue_id is not None
    labels = {
        "created-by": "clipse",
        "repo": repo_label(request.repo_slug),
        "issue": request.issue_id,
        "role": request.role,
    }
    if request.role == "reviewer":
        assert request.run_id is not None
        labels["run"] = request.run_id
    return labels


def _state_value(state: Any) -> WorkspaceState:
    value = getattr(state, "value", state)
    if value == "started":
        return "active"
    if value in {"stopped", "stopping", "archived", "paused", "pausing"}:
        return "stopped"
    if value in {"destroying", "archiving"}:
        return "cleanup_pending"
    if value == "destroyed":
        return "deleted"
    return "error"


def workspace_from(
    sandbox: _Sandbox,
    request: BackendActionRequest,
    *,
    state: WorkspaceState | None = None,
) -> BackendWorkspace:
    """Convert a Daytona SDK object into the stable public contract."""

    return BackendWorkspace(
        external_id=sandbox.id,
        state=state or _state_value(sandbox.state),
        workspace_path=REMOTE_REPO_ABS,
        owner_key=owner_key(request),
    )


def _repo_labels(repo_slug: str) -> dict[str, str]:
    return {"created-by": "clipse", "repo": repo_label(repo_slug)}


def _owner_from_labels(sandbox: _Sandbox, repo_slug: str) -> str:
    labels = getattr(sandbox, "labels", {})
    role = labels.get("role")
    issue_id = labels.get("issue")
    run_id = labels.get("run")
    if role not in {"coder", "reviewer"} or not issue_id or (role == "reviewer" and not run_id):
        raise BackendActionError("needs_input", "list", f"sandbox {sandbox.id} has incomplete clipse labels")
    base = f"daytona:{repo_slug}:{role}:{issue_id}"
    return f"{base}:{run_id}" if role == "reviewer" else base


def workspace_from_labels(sandbox: _Sandbox, repo_slug: str) -> BackendWorkspace:
    return BackendWorkspace(
        external_id=sandbox.id,
        state=_state_value(sandbox.state),
        workspace_path=REMOTE_REPO_ABS,
        owner_key=_owner_from_labels(sandbox, repo_slug),
    )


def _default_client_factory(request: BackendActionRequest) -> _DaytonaClient:
    return Daytona(DaytonaConfig(target=request.target))


class DaytonaLifecycle:
    """Ensure, enumerate, and delete Clipse-owned Daytona sandboxes."""

    def __init__(
        self,
        client_factory: ClientFactory = _default_client_factory,
        token_reader: TokenReader = github_token,
        auth_preflight: AuthPreflight = github_auth_preflight,
    ) -> None:
        self._client_factory = client_factory
        self._token_reader = token_reader
        self._auth_preflight = auth_preflight
        self._client: _DaytonaClient | None = None

    def _prepare(self, request: BackendActionRequest) -> None:
        try:
            self._client = self._client_factory(request)
        except (DaytonaNotFoundError, DaytonaValidationError) as exc:
            raise BackendActionError(
                "needs_input",
                "daytona_config",
                safe_error("daytona configuration", exc),
            ) from None

    def _matching(self, request: BackendActionRequest) -> list[_Sandbox]:
        assert self._client is not None
        return list(self._client.list(ListSandboxesQuery(labels=labels_for(request))))

    def _matching_repo(self, request: BackendActionRequest) -> list[_Sandbox]:
        assert self._client is not None
        return list(self._client.list(ListSandboxesQuery(labels=_repo_labels(request.repo_slug))))

    def _repo_ready(self, sandbox: _Sandbox) -> bool:
        try:
            sandbox.fs.get_file_info(f"{REMOTE_REPO_REL}/.git")
        except DaytonaNotFoundError:
            assert self._client is not None
            try:
                self._client.get(sandbox.id)
            except DaytonaNotFoundError:
                raise BackendActionError("transient", "ensure", "sandbox vanished during ensure") from None
            return False
        except FileNotFoundError:
            return False
        return True

    def _create_and_clone(self, request: BackendActionRequest, token: str) -> _Sandbox:
        assert self._client is not None
        assert request.role is not None
        assert request.auto_stop_minutes is not None
        assert request.reviewer_auto_delete_minutes is not None
        assert request.repo_url is not None
        assert request.branch is not None
        assert request.base_branch is not None
        auto_delete = request.reviewer_auto_delete_minutes if request.role == "reviewer" else None
        params = CreateSandboxFromSnapshotParams(
            name=owner_key(request),
            snapshot=request.snapshot,
            env_vars={},
            labels=labels_for(request),
            auto_stop_interval=request.auto_stop_minutes,
            auto_delete_interval=auto_delete,
        )
        try:
            sandbox = self._client.create(params)
        except (DaytonaNotFoundError, DaytonaValidationError) as exc:
            raise BackendActionError(
                "needs_input",
                "daytona_config",
                safe_error("daytona configuration", exc),
            ) from None
        clone_branch = request.branch if request.role == "reviewer" else request.base_branch
        try:
            sandbox.git.clone(
                url=request.repo_url,
                path=REMOTE_REPO_REL,
                branch=clone_branch,
                username="x-access-token",
                password=token,
            )
        except Exception as exc:
            try:
                self._client.delete(sandbox)
            except Exception as cleanup_exc:
                message = (
                    f"clone failed and cleanup failed for sandbox {sandbox.id} "
                    f"({type(cleanup_exc).__name__})"
                )
                raise BackendActionError("needs_input", "clone_cleanup", message) from None
            raise BackendActionError("transient", "clone", safe_error("clone", exc)) from None
        return sandbox

    def ensure(self, request: BackendActionRequest) -> BackendWorkspace:
        self._prepare(request)
        try:
            matches = self._matching(request)
        except DaytonaNotFoundError:
            raise BackendActionError("transient", "ensure", "sandbox vanished during ensure") from None
        if len(matches) > 1:
            ids = ", ".join(sorted(item.id for item in matches))
            raise BackendActionError("needs_input", "ensure", f"multiple matching sandboxes: {ids}")
        if len(matches) == 1:
            sandbox = matches[0]
            if sandbox.state != "started":
                assert self._client is not None
                try:
                    self._client.start(sandbox)
                except DaytonaNotFoundError:
                    raise BackendActionError("transient", "ensure", "sandbox vanished during ensure") from None
            if not self._repo_ready(sandbox):
                token = self._token_reader()
                self._client.delete(sandbox)
                sandbox = self._create_and_clone(request, token)
        else:
            token = self._token_reader()
            sandbox = self._create_and_clone(request, token)
        return workspace_from(sandbox, request)

    def delete(self, request: BackendActionRequest) -> BackendWorkspace:
        self._prepare(request)
        if not request.sandbox_id:
            raise BackendActionError("needs_input", "delete", "sandbox id is required")
        assert self._client is not None
        try:
            sandbox = self._client.get(request.sandbox_id)
        except DaytonaNotFoundError:
            return BackendWorkspace(
                external_id=request.sandbox_id,
                state="deleted",
                workspace_path=REMOTE_REPO_ABS,
                owner_key=owner_key(request),
            )
        try:
            self._client.delete(sandbox)
        except DaytonaNotFoundError:
            pass
        return workspace_from(sandbox, request, state="deleted")

    def list(self, request: BackendActionRequest) -> list[BackendWorkspace]:
        self._prepare(request)
        matches = self._matching_repo(request)
        self._auth_preflight()
        return [workspace_from_labels(sandbox, request.repo_slug) for sandbox in matches]


__all__ = [
    "REMOTE_REPO_ABS",
    "REMOTE_REPO_REL",
    "BackendActionError",
    "DaytonaLifecycle",
    "labels_for",
    "owner_key",
    "repo_label",
    "workspace_from",
    "workspace_from_labels",
]
