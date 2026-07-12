"""Fakeable Daytona sandbox lifecycle operations."""

from __future__ import annotations

import hashlib
import posixpath
import shlex
from collections.abc import Awaitable, Callable, Iterable, Sequence
from dataclasses import dataclass, field
from typing import Any, Protocol, TypeVar

from daytona import (
    CreateSandboxFromSnapshotParams,
    Daytona,
    DaytonaAuthenticationError,
    DaytonaAuthorizationError,
    DaytonaConfig,
    DaytonaNotFoundError,
    DaytonaValidationError,
    ListSandboxesQuery,
)
from deepagents.backends.protocol import (
    EditResult,
    ExecuteResponse,
    FileDownloadResponse,
    FileUploadResponse,
    GlobResult,
    GrepResult,
    LsResult,
    ReadResult,
    SandboxBackendProtocol,
    WriteResult,
)

from clipse_agent.backends.contracts import (
    BackendActionError,
    BackendActionRequest,
    BackendWorkspace,
    WorkspaceState,
)
from clipse_agent.backends.github import (
    AuthPreflight,
    BranchExists,
    canonical_github_command,
    github_auth_preflight,
    github_branch_exists,
    github_token,
    safe_error,
)
from clipse_agent.backends.github import HostRunner, subprocess_host_runner
from clipse_agent.backends.session import CommandResult

REMOTE_REPO_REL = "workspace/clipse"
REMOTE_REPO_ABS = "/home/daytona/workspace/clipse"


class _Git(Protocol):
    def clone(self, **kwargs: Any) -> None: ...

    def add(self, path: str, files: list[str]) -> None: ...

    def status(self, path: str) -> Any: ...

    def create_branch(self, path: str, name: str) -> None: ...

    def checkout_branch(self, path: str, branch: str) -> None: ...

    def branches(self, path: str) -> Any: ...


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

_GIT_USERNAME = "x-access-token"
GIT_AUTHOR_NAME = "clipse"
GIT_AUTHOR_EMAIL = "clipse@users.noreply.github.com"
_BACKEND_FAILURE = "Daytona backend operation failed"
_T = TypeVar("_T")


class RepositoryScopedDaytonaSandbox(SandboxBackendProtocol):
    """Scope Daytona shell and file operations to one absolute repository.

    DAC's backend protocol accepts relative paths even though Daytona's native
    file transfer methods require absolute paths. This adapter owns that
    translation consistently across the complete pinned sync/async protocol.
    Absolute paths are preserved for deliberate sandbox-global access.
    """

    def __init__(self, backend: SandboxBackendProtocol, cwd: str) -> None:
        normalized = posixpath.normpath(cwd)
        if not posixpath.isabs(normalized):
            raise ValueError("repository root must be absolute")
        self.backend = backend
        self.cwd = normalized

    @property
    def id(self) -> str:
        return self.backend.id

    def _path(self, path: str) -> str:
        if posixpath.isabs(path):
            return path
        resolved = posixpath.normpath(posixpath.join(self.cwd, path))
        if posixpath.commonpath((self.cwd, resolved)) != self.cwd:
            raise ValueError("relative path escapes repository root")
        return resolved

    def _safe(self, operation: Callable[[], _T], fallback: Callable[[], _T]) -> _T:
        try:
            return operation()
        except Exception:  # noqa: BLE001 - provider exceptions may contain response bodies or credentials
            return fallback()

    async def _safe_async(
        self,
        operation: Callable[[], Awaitable[_T]],
        fallback: Callable[[], _T],
    ) -> _T:
        try:
            return await operation()
        except Exception:  # noqa: BLE001 - provider exceptions may contain response bodies or credentials
            return fallback()

    def _command(self, command: str) -> str:
        return f"cd {shlex.quote(self.cwd)} && {command}"

    def execute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        scoped = self._command(command)
        return self._safe(
            lambda: self.backend.execute(scoped, timeout=timeout),
            lambda: ExecuteResponse(output=_BACKEND_FAILURE, exit_code=1),
        )

    async def aexecute(self, command: str, *, timeout: int | None = None) -> ExecuteResponse:
        scoped = self._command(command)
        return await self._safe_async(
            lambda: self.backend.aexecute(scoped, timeout=timeout),
            lambda: ExecuteResponse(output=_BACKEND_FAILURE, exit_code=1),
        )

    def ls(self, path: str) -> LsResult:
        resolved = self._path(path)
        return self._safe(
            lambda: self.backend.ls(resolved),
            lambda: LsResult(error=_BACKEND_FAILURE),
        )

    async def als(self, path: str) -> LsResult:
        resolved = self._path(path)
        return await self._safe_async(
            lambda: self.backend.als(resolved),
            lambda: LsResult(error=_BACKEND_FAILURE),
        )

    def read(self, file_path: str, offset: int = 0, limit: int = 2000) -> ReadResult:
        resolved = self._path(file_path)
        return self._safe(
            lambda: self.backend.read(resolved, offset, limit),
            lambda: ReadResult(error=_BACKEND_FAILURE),
        )

    async def aread(self, file_path: str, offset: int = 0, limit: int = 2000) -> ReadResult:
        resolved = self._path(file_path)
        return await self._safe_async(
            lambda: self.backend.aread(resolved, offset, limit),
            lambda: ReadResult(error=_BACKEND_FAILURE),
        )

    def grep(
        self,
        pattern: str,
        path: str | None = None,
        glob: str | None = None,
    ) -> GrepResult:
        resolved = self.cwd if path is None else self._path(path)
        return self._safe(
            lambda: self.backend.grep(pattern, resolved, glob),
            lambda: GrepResult(error=_BACKEND_FAILURE),
        )

    async def agrep(
        self,
        pattern: str,
        path: str | None = None,
        glob: str | None = None,
    ) -> GrepResult:
        resolved = self.cwd if path is None else self._path(path)
        return await self._safe_async(
            lambda: self.backend.agrep(pattern, resolved, glob),
            lambda: GrepResult(error=_BACKEND_FAILURE),
        )

    def glob(self, pattern: str, path: str | None = None) -> GlobResult:
        resolved = self.cwd if path is None else self._path(path)
        return self._safe(
            lambda: self.backend.glob(pattern, resolved),
            lambda: GlobResult(error=_BACKEND_FAILURE),
        )

    async def aglob(self, pattern: str, path: str | None = None) -> GlobResult:
        resolved = self.cwd if path is None else self._path(path)
        return await self._safe_async(
            lambda: self.backend.aglob(pattern, resolved),
            lambda: GlobResult(error=_BACKEND_FAILURE),
        )

    def write(self, file_path: str, content: str) -> WriteResult:
        resolved = self._path(file_path)
        return self._safe(
            lambda: self.backend.write(resolved, content),
            lambda: WriteResult(error=_BACKEND_FAILURE),
        )

    async def awrite(self, file_path: str, content: str) -> WriteResult:
        resolved = self._path(file_path)
        return await self._safe_async(
            lambda: self.backend.awrite(resolved, content),
            lambda: WriteResult(error=_BACKEND_FAILURE),
        )

    def edit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        resolved = self._path(file_path)
        return self._safe(
            lambda: self.backend.edit(resolved, old_string, new_string, replace_all),
            lambda: EditResult(error=_BACKEND_FAILURE),
        )

    async def aedit(
        self,
        file_path: str,
        old_string: str,
        new_string: str,
        replace_all: bool = False,
    ) -> EditResult:
        resolved = self._path(file_path)
        return await self._safe_async(
            lambda: self.backend.aedit(resolved, old_string, new_string, replace_all),
            lambda: EditResult(error=_BACKEND_FAILURE),
        )

    def upload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        resolved = [(self._path(path), content) for path, content in files]
        return self._safe(
            lambda: self.backend.upload_files(resolved),
            lambda: [FileUploadResponse(path=path, error=_BACKEND_FAILURE) for path, _ in resolved],
        )

    async def aupload_files(self, files: list[tuple[str, bytes]]) -> list[FileUploadResponse]:
        resolved = [(self._path(path), content) for path, content in files]
        return await self._safe_async(
            lambda: self.backend.aupload_files(resolved),
            lambda: [FileUploadResponse(path=path, error=_BACKEND_FAILURE) for path, _ in resolved],
        )

    def download_files(self, paths: list[str]) -> list[FileDownloadResponse]:
        resolved = [self._path(path) for path in paths]
        return self._safe(
            lambda: self.backend.download_files(resolved),
            lambda: [FileDownloadResponse(path=path, error=_BACKEND_FAILURE) for path in resolved],
        )

    async def adownload_files(self, paths: list[str]) -> list[FileDownloadResponse]:
        resolved = [self._path(path) for path in paths]
        return await self._safe_async(
            lambda: self.backend.adownload_files(resolved),
            lambda: [FileDownloadResponse(path=path, error=_BACKEND_FAILURE) for path in resolved],
        )


@dataclass(frozen=True)
class DaytonaSession:
    """Execute a worker turn against one existing Daytona sandbox.

    ``sandbox`` is the LangChain ``DaytonaSandbox`` passed to DAC. The raw
    SDK sandbox is retained separately for credential-scoped Git API calls;
    GitHub CLI calls stay on the host and are scoped explicitly to
    ``repo_slug`` so they never depend on a host checkout.
    """

    cwd: str
    repo_slug: str
    sandbox: Any
    sdk_sandbox: Any
    token_reader: TokenReader = github_token
    host_runner: HostRunner = subprocess_host_runner
    provider: str = field(default="daytona", init=False)
    sandbox_type: str = field(default="daytona", init=False)

    @property
    def repo_path(self) -> str:
        """Absolute repository path used by Daytona's Git SDK."""

        return self.cwd

    def run(self, argv: Sequence[str]) -> CommandResult:
        try:
            response = self.sandbox.execute(shlex.join(argv))
        except Exception:  # noqa: BLE001 - provider exceptions may contain response bodies or credentials
            return CommandResult(1, stderr="Daytona command failed")
        returncode = response.exit_code if response.exit_code is not None else 1
        return CommandResult(returncode=returncode, stdout=response.output)

    def _git_token(self, operation: str) -> tuple[str | None, CommandResult | None]:
        try:
            return self.token_reader(), None
        except Exception as exc:  # noqa: BLE001 - sanitize all credential-helper failures
            return None, CommandResult(1, stderr=safe_error(operation, exc))

    def sync_base(self, base_branch: str) -> CommandResult:
        token, failure = self._git_token("git pull authentication")
        if failure is not None:
            return failure
        try:
            self.sdk_sandbox.git.pull(
                self.repo_path,
                username=_GIT_USERNAME,
                password=token,
                branch=base_branch,
                remote="origin",
            )
        except Exception as exc:  # noqa: BLE001 - SDK errors may contain credentials
            return CommandResult(1, stderr=safe_error("git pull", exc))
        return CommandResult(0)

    def commit(self, message: str) -> CommandResult:
        try:
            self.sdk_sandbox.git.add(self.repo_path, ["."])
            self.sdk_sandbox.git.commit(
                path=self.repo_path,
                message=message,
                author=GIT_AUTHOR_NAME,
                email=GIT_AUTHOR_EMAIL,
            )
        except Exception as exc:  # noqa: BLE001 - keep provider details out of results
            return CommandResult(1, stderr=safe_error("git commit", exc))
        return CommandResult(0)

    def commit_merge(self) -> CommandResult:
        return self.run(
            [
                "git",
                "-c",
                f"user.name={GIT_AUTHOR_NAME}",
                "-c",
                f"user.email={GIT_AUTHOR_EMAIL}",
                "commit",
                "--no-edit",
            ]
        )

    def push(self, branch: str) -> CommandResult:
        token, failure = self._git_token("git push authentication")
        if failure is not None:
            return failure
        try:
            self.sdk_sandbox.git.push(
                path=self.repo_path,
                username=_GIT_USERNAME,
                password=token,
                branch=branch,
                remote="origin",
                set_upstream=True,
            )
        except Exception as exc:  # noqa: BLE001 - SDK errors may contain credentials
            return CommandResult(1, stderr=safe_error("git push", exc))
        return CommandResult(0)

    def github(self, argv: Sequence[str]) -> CommandResult:
        command = canonical_github_command(argv, self.repo_slug)
        try:
            stdout = self.host_runner(command)
        except BackendActionError as exc:
            return CommandResult(1, stderr=str(exc))
        except Exception as exc:  # noqa: BLE001 - never echo host command exception text
            return CommandResult(1, stderr=safe_error("GitHub command", exc))
        return CommandResult(0, stdout=stdout)


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
        branch_exists: BranchExists = github_branch_exists,
    ) -> None:
        self._client_factory = client_factory
        self._token_reader = token_reader
        self._auth_preflight = auth_preflight
        self._branch_exists = branch_exists
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

    @staticmethod
    def _failure_kind(exc: BaseException) -> str:
        if isinstance(exc, BackendActionError):
            return exc.kind
        if isinstance(
            exc,
            (DaytonaAuthenticationError, DaytonaAuthorizationError, DaytonaValidationError),
        ):
            return "needs_input"
        return "transient"

    @classmethod
    def _combined_failure_kind(cls, *errors: BaseException) -> str:
        if any(cls._failure_kind(error) == "needs_input" for error in errors):
            return "needs_input"
        return "transient"

    def _remote_feature_exists(self, request: BackendActionRequest) -> bool:
        assert request.branch is not None
        try:
            return self._branch_exists(request.repo_slug, request.branch)
        except BackendActionError:
            raise
        except Exception as exc:  # noqa: BLE001 - sanitize host gh boundary
            raise BackendActionError(
                "transient", "github_branch", safe_error("GitHub branch lookup", exc)
            ) from None

    def _create_and_clone(
        self,
        request: BackendActionRequest,
        token: str,
        *,
        remote_feature_exists: bool | None = None,
    ) -> _Sandbox:
        assert self._client is not None
        assert request.role is not None
        assert request.auto_stop_minutes is not None
        assert request.reviewer_auto_delete_minutes is not None
        assert request.repo_url is not None
        assert request.branch is not None
        assert request.base_branch is not None
        if request.role == "reviewer":
            clone_branch = request.branch
        else:
            exists = self._remote_feature_exists(request) if remote_feature_exists is None else remote_feature_exists
            clone_branch = request.branch if exists else request.base_branch
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
        try:
            sandbox.git.clone(
                url=request.repo_url,
                path=REMOTE_REPO_REL,
                branch=clone_branch,
                username="x-access-token",
                password=token,
            )
            if request.role == "coder" and clone_branch != request.branch:
                self._establish_local_feature_branch(sandbox, request.branch)
        except Exception as exc:
            try:
                self._client.delete(sandbox)
            except Exception as cleanup_exc:
                message = (
                    f"clone failed and cleanup failed for sandbox {sandbox.id} "
                    f"({type(cleanup_exc).__name__})"
                )
                raise BackendActionError(
                    self._combined_failure_kind(exc, cleanup_exc), "clone_cleanup", message
                ) from None
            raise BackendActionError(self._failure_kind(exc), "clone", safe_error("clone", exc)) from None
        return sandbox

    def _branch_state(self, sandbox: _Sandbox) -> tuple[str, set[str]]:
        try:
            current = sandbox.git.status(REMOTE_REPO_ABS).current_branch
            raw_branches = sandbox.git.branches(REMOTE_REPO_ABS).branches
        except Exception as exc:  # noqa: BLE001 - provider bodies may contain credentials
            raise BackendActionError(
                self._failure_kind(exc), "ensure_branch", safe_error("branch state", exc)
            ) from None
        if not isinstance(current, str) or not isinstance(raw_branches, list) or not all(
            isinstance(branch, str) for branch in raw_branches
        ):
            raise BackendActionError("transient", "ensure_branch", "branch state was malformed")
        return current, set(raw_branches)

    def _checkout_local_feature(self, sandbox: _Sandbox, branch: str) -> None:
        try:
            sandbox.git.checkout_branch(REMOTE_REPO_ABS, branch)
        except Exception as exc:  # noqa: BLE001 - verify ambiguous provider responses
            current, _branches = self._branch_state(sandbox)
            if current == branch:
                return
            raise BackendActionError(
                self._failure_kind(exc), "ensure_branch", safe_error("branch checkout", exc)
            ) from None
        current, _branches = self._branch_state(sandbox)
        if current != branch:
            raise BackendActionError("transient", "ensure_branch", "branch checkout did not take effect")

    def _establish_local_feature_branch(self, sandbox: _Sandbox, branch: str) -> None:
        current, branches = self._branch_state(sandbox)
        if current == branch:
            return
        if branch in branches:
            self._checkout_local_feature(sandbox, branch)
            return
        try:
            sandbox.git.create_branch(REMOTE_REPO_ABS, branch)
        except Exception as exc:  # noqa: BLE001 - create may have succeeded before response loss
            current, branches = self._branch_state(sandbox)
            if current == branch:
                return
            if branch in branches:
                self._checkout_local_feature(sandbox, branch)
                return
            raise BackendActionError(
                self._failure_kind(exc), "ensure_branch", safe_error("branch creation", exc)
            ) from None
        current, branches = self._branch_state(sandbox)
        if current == branch:
            return
        if branch not in branches:
            raise BackendActionError("transient", "ensure_branch", "branch creation did not take effect")
        self._checkout_local_feature(sandbox, branch)

    def _ensure_coder_branch(self, sandbox: _Sandbox, request: BackendActionRequest) -> _Sandbox:
        assert request.branch is not None
        current, _branches = self._branch_state(sandbox)
        if current == request.branch:
            return sandbox

        remote_exists = self._remote_feature_exists(request)
        if not remote_exists:
            self._establish_local_feature_branch(sandbox, request.branch)
            return sandbox

        # A previous buggy provision left this identity on the base branch.
        # Recreate from the pushed feature rather than guessing how much state
        # the incomplete clone has fetched.
        assert self._client is not None
        token = self._token_reader()
        try:
            self._client.delete(sandbox)
        except Exception as exc:  # noqa: BLE001
            raise BackendActionError(
                self._failure_kind(exc), "ensure_branch", safe_error("sandbox recreation", exc)
            ) from None
        return self._create_and_clone(request, token, remote_feature_exists=True)

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
        if request.role == "coder":
            sandbox = self._ensure_coder_branch(sandbox, request)
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
    "DaytonaSession",
    "RepositoryScopedDaytonaSandbox",
    "labels_for",
    "owner_key",
    "repo_label",
    "workspace_from",
    "workspace_from_labels",
]
