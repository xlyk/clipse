"""Secret-safe access to the host's existing GitHub CLI authentication."""

from __future__ import annotations

import subprocess
import json
from collections.abc import Callable, Sequence
from urllib.parse import quote

from clipse_agent.backends.contracts import BackendActionError

HostRunner = Callable[[list[str]], str]
AuthPreflight = Callable[[], None]
BranchExists = Callable[[str, str], bool]


def _operation(argv: Sequence[str]) -> str:
    return " ".join(argv[:3]) or "host command"


def canonical_github_command(argv: Sequence[str], repo_slug: str) -> list[str]:
    """Return one host ``gh`` command pinned to the configured repository.

    Repository-aware ``gh`` subcommands receive exactly one canonical
    ``--repo`` argument. ``gh api`` has no ``--repo`` option, so its standard
    owner/repository placeholders are expanded before the command reaches the
    host instead of relying on whichever checkout happens to be current.
    """

    command = list(argv)
    if not command or command[0] != "gh":
        command.insert(0, "gh")

    scoped: list[str] = []
    skip_value = False
    for arg in command:
        if skip_value:
            skip_value = False
            continue
        if arg in {"--repo", "-R"}:
            skip_value = True
            continue
        if arg.startswith("--repo="):
            continue
        scoped.append(arg)

    if len(scoped) > 1 and scoped[1] == "api":
        owner, repo = repo_slug.split("/", 1)
        if len(scoped) > 2:
            scoped[2] = scoped[2].replace("{owner}", owner).replace("{repo}", repo)
        return scoped

    scoped.extend(["--repo", repo_slug])
    return scoped


def subprocess_host_runner(argv: list[str]) -> str:
    """Run a host command without propagating its potentially secret output."""

    operation = _operation(argv)
    try:
        completed = subprocess.run(argv, capture_output=True, check=True, text=True)
    except subprocess.CalledProcessError as exc:
        message = f"{operation} exited with status {exc.returncode}"
        if "no pull requests found" in (exc.stderr or "").lower():
            # Preserve the reviewer's intentional no-PR fallback without
            # forwarding arbitrary stderr (which may contain credentials).
            message = "no pull requests found"
        raise BackendActionError(
            "needs_input",
            operation,
            message,
        ) from None
    except OSError:
        raise BackendActionError("needs_input", operation, f"{operation} could not be executed") from None
    return completed.stdout.strip()


def github_auth_preflight(run_host: HostRunner = subprocess_host_runner) -> None:
    """Verify host GitHub auth without reading or materializing its token."""

    run_host(["gh", "auth", "status", "--hostname", "github.com"])


def github_token(run_host: HostRunner = subprocess_host_runner) -> str:
    """Read a GitHub token only after verifying the host CLI's auth state."""

    github_auth_preflight(run_host)
    token = run_host(["gh", "auth", "token", "--hostname", "github.com"])
    if not token:
        raise BackendActionError("needs_input", "github_auth", "gh auth token returned empty")
    return token


def github_branch_exists(repo_slug: str, branch: str) -> bool:
    """Return whether GitHub has exactly ``refs/heads/<branch>``.

    The matching-refs endpoint returns a successful empty array when a ref is
    absent. Every command/protocol failure is instead a sanitized transient;
    it must never be mistaken for "branch missing" and cause a divergent
    feature branch to be created from the base.
    """

    endpoint = f"repos/{repo_slug}/git/matching-refs/heads/{quote(branch, safe='')}"
    try:
        completed = subprocess.run(
            ["gh", "api", endpoint],
            capture_output=True,
            check=True,
            text=True,
        )
        payload = json.loads(completed.stdout)
    except (subprocess.CalledProcessError, OSError, json.JSONDecodeError):
        raise BackendActionError("transient", "github_branch", "GitHub branch lookup failed") from None
    if not isinstance(payload, list):
        raise BackendActionError("transient", "github_branch", "GitHub branch lookup failed")
    refs: list[str] = []
    for item in payload:
        if not isinstance(item, dict) or not isinstance(item.get("ref"), str):
            raise BackendActionError("transient", "github_branch", "GitHub branch lookup failed")
        refs.append(item["ref"])
    exact = f"refs/heads/{branch}"
    return exact in refs


def safe_error(operation: str, exc: BaseException) -> str:
    """Describe a failure without ever echoing exception text or credentials."""

    return f"{operation} failed ({type(exc).__name__})"


__all__ = [
    "AuthPreflight",
    "BranchExists",
    "BackendActionError",
    "HostRunner",
    "canonical_github_command",
    "github_auth_preflight",
    "github_branch_exists",
    "github_token",
    "safe_error",
    "subprocess_host_runner",
]
