"""Typed input and output contracts for backend lifecycle actions."""

from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict

BackendAction = Literal["ensure", "delete", "list"]
BackendProvider = Literal["daytona"]
BackendRole = Literal["coder", "reviewer"]
ErrorKind = Literal["transient", "capability", "needs_input"]
WorkspaceState = Literal[
    "creating",
    "restoring",
    "destroyed",
    "destroying",
    "started",
    "stopped",
    "starting",
    "stopping",
    "error",
    "build_failed",
    "pending_build",
    "building_snapshot",
    "unknown",
    "pulling_snapshot",
    "archived",
    "archiving",
    "resizing",
    "snapshotting",
    "forking",
    "pausing",
    "paused",
    "resuming",
]


class BackendActionError(RuntimeError):
    """Categorized lifecycle failure safe for the deterministic caller."""

    def __init__(self, kind: ErrorKind, operation: str, message: str) -> None:
        super().__init__(message)
        self.kind = kind
        self.operation = operation


class BackendActionRequest(BaseModel):
    """Everything needed to perform one backend lifecycle action."""

    model_config = ConfigDict(extra="forbid")

    action: BackendAction
    provider: BackendProvider
    repo_url: str
    repo_slug: str
    base_branch: str
    branch: str
    issue_id: str
    run_id: str
    role: BackendRole
    auto_stop_minutes: int
    reviewer_auto_delete_minutes: int
    sandbox_id: str | None = None
    snapshot: str | None = None
    target: str | None = None


class BackendWorkspace(BaseModel):
    """Serializable identity and repository location of a remote workspace."""

    model_config = ConfigDict(extra="forbid")

    id: str
    state: WorkspaceState
    path: str
    owner: str


class BackendActionResult(BaseModel):
    """One-line JSON result consumed by the Go lifecycle caller."""

    model_config = ConfigDict(extra="forbid")

    action: BackendAction
    provider: BackendProvider
    ok: bool
    workspace: BackendWorkspace | None = None
    workspaces: list[BackendWorkspace] | None = None
    error_kind: ErrorKind | None = None
    error_operation: str | None = None
    error_message: str | None = None
