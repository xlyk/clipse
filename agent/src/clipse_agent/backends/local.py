"""Host-local runtime session."""

from __future__ import annotations

import subprocess
from collections.abc import Sequence
from dataclasses import dataclass, field
from typing import Any

from clipse_agent.backends.session import CommandResult


@dataclass(frozen=True)
class LocalSession:
    """Run agent and repository operations in the kernel-owned worktree."""

    cwd: str
    repo_slug: str
    provider: str = field(default="local", init=False)
    sandbox: Any | None = field(default=None, init=False)
    sandbox_type: str | None = field(default=None, init=False)

    def run(self, argv: Sequence[str]) -> CommandResult:
        completed = subprocess.run(list(argv), cwd=self.cwd, capture_output=True, text=True)
        return CommandResult(
            returncode=completed.returncode,
            stdout=completed.stdout,
            stderr=completed.stderr,
        )

    def sync_base(self, base_branch: str) -> CommandResult:
        fetched = self.run(["git", "fetch", "origin", base_branch])
        if fetched.returncode != 0:
            return fetched
        return self.run(["git", "merge", "--no-edit", f"origin/{base_branch}"])

    def commit(self, message: str) -> CommandResult:
        return self.run(["git", "commit", "-m", message])

    def commit_merge(self) -> CommandResult:
        return self.run(["git", "commit", "--no-edit"])

    def push(self, branch: str) -> CommandResult:
        return self.run(["git", "push", "--set-upstream", "origin", branch])

    def github(self, argv: Sequence[str]) -> CommandResult:
        command = list(argv)
        if not command or command[0] != "gh":
            command.insert(0, "gh")
        return self.run(command)


__all__ = ["LocalSession"]
