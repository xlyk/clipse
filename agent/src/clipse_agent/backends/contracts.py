"""Typed input and output contracts for backend lifecycle actions."""

from __future__ import annotations

from typing import Annotated, Literal, Self

from pydantic import BaseModel, ConfigDict, Field, StringConstraints, model_validator

BackendAction = Literal["ensure", "delete", "list"]
BackendProvider = Literal["daytona"]
BackendRole = Literal["coder", "reviewer"]
ErrorKind = Literal["transient", "capability", "needs_input"]
WorkspaceState = Literal["active", "stopped", "cleanup_pending", "deleted", "error"]
NonEmptyStr = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1)]
PositiveInt = Annotated[int, Field(gt=0)]


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
    repo_slug: NonEmptyStr
    repo_url: NonEmptyStr | None = None
    base_branch: NonEmptyStr | None = None
    branch: NonEmptyStr | None = None
    issue_id: NonEmptyStr | None = None
    run_id: NonEmptyStr | None = None
    role: BackendRole | None = None
    auto_stop_minutes: PositiveInt | None = None
    reviewer_auto_delete_minutes: PositiveInt | None = None
    sandbox_id: NonEmptyStr | None = None
    snapshot: NonEmptyStr | None = None
    target: NonEmptyStr | None = None

    @model_validator(mode="after")
    def validate_action_scope(self) -> Self:
        if self.action == "list":
            return self
        scoped = ["issue_id", "role"]
        if self.action == "ensure" or self.role == "reviewer":
            scoped.append("run_id")
        missing = [field for field in scoped if getattr(self, field) is None]
        if self.action == "delete":
            if self.sandbox_id is None:
                missing.append("sandbox_id")
        else:
            provision = (
                "repo_url",
                "base_branch",
                "branch",
                "auto_stop_minutes",
                "reviewer_auto_delete_minutes",
            )
            missing.extend(field for field in provision if getattr(self, field) is None)
        if missing:
            raise ValueError(f"{self.action} requires: {', '.join(missing)}")
        return self


class BackendWorkspace(BaseModel):
    """Serializable identity and repository location of a remote workspace."""

    model_config = ConfigDict(extra="forbid")

    external_id: NonEmptyStr
    state: WorkspaceState
    workspace_path: NonEmptyStr
    owner_key: NonEmptyStr


class BackendActionResult(BaseModel):
    """One-line JSON result consumed by the Go lifecycle caller."""

    model_config = ConfigDict(extra="forbid")

    action: BackendAction
    provider: BackendProvider
    ok: bool
    owner_key: NonEmptyStr | None = None
    external_id: NonEmptyStr | None = None
    workspace_path: NonEmptyStr | None = None
    state: WorkspaceState | None = None
    workspaces: list[BackendWorkspace] | None = None
    error_kind: ErrorKind | None = None
    error_operation: NonEmptyStr | None = None
    error: NonEmptyStr | None = None

    @model_validator(mode="after")
    def validate_result_state(self) -> Self:
        success_fields = (self.owner_key, self.external_id, self.workspace_path, self.state)
        has_error = self.error_kind is not None or self.error_operation is not None or self.error is not None
        if self.ok:
            if has_error:
                raise ValueError("successful backend result cannot contain error fields")
            if self.action == "list":
                if self.workspaces is None or any(value is not None for value in success_fields):
                    raise ValueError("successful list result requires only workspaces")
            elif self.workspaces is not None or any(value is None for value in success_fields):
                raise ValueError("successful lifecycle result requires top-level workspace fields")
            return self
        if self.error_kind is None or self.error_operation is None or self.error is None:
            raise ValueError("failed backend result requires error_kind, error_operation, and error")
        if self.workspaces is not None or any(value is not None for value in success_fields):
            raise ValueError("failed backend result cannot contain workspace fields")
        return self
