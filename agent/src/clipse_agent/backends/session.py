"""Runtime session contract shared by the worker and lane graphs."""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any, Protocol


@dataclass(frozen=True)
class CommandResult:
    """The outcome of one backend command."""

    returncode: int
    stdout: str = ""
    stderr: str = ""


class AgentSession(Protocol):
    """Graph-facing execution and repository session."""

    provider: str
    cwd: str
    sandbox: Any | None
    sandbox_type: str | None

    def run(self, argv: Sequence[str]) -> CommandResult:
        raise NotImplementedError

    def sync_base(self, base_branch: str) -> CommandResult:
        raise NotImplementedError

    def commit(self, message: str) -> CommandResult:
        raise NotImplementedError

    def push(self, branch: str) -> CommandResult:
        raise NotImplementedError

    def github(self, argv: Sequence[str]) -> CommandResult:
        raise NotImplementedError


__all__ = ["AgentSession", "CommandResult"]
