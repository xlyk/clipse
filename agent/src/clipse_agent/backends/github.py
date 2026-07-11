"""Secret-safe access to the host's existing GitHub CLI authentication."""

from __future__ import annotations

import subprocess
from collections.abc import Callable, Sequence

from clipse_agent.backends.contracts import BackendActionError

HostRunner = Callable[[list[str]], str]


def _operation(argv: Sequence[str]) -> str:
    return " ".join(argv[:3]) or "host command"


def subprocess_host_runner(argv: list[str]) -> str:
    """Run a host command without propagating its potentially secret output."""

    operation = _operation(argv)
    try:
        completed = subprocess.run(argv, capture_output=True, check=True, text=True)
    except subprocess.CalledProcessError as exc:
        raise BackendActionError(
            "needs_input",
            operation,
            f"{operation} exited with status {exc.returncode}",
        ) from None
    except OSError:
        raise BackendActionError("needs_input", operation, f"{operation} could not be executed") from None
    return completed.stdout.strip()


def github_token(run_host: HostRunner = subprocess_host_runner) -> str:
    """Read a GitHub token only after verifying the host CLI's auth state."""

    run_host(["gh", "auth", "status", "--hostname", "github.com"])
    token = run_host(["gh", "auth", "token", "--hostname", "github.com"])
    if not token:
        raise BackendActionError("needs_input", "github_auth", "gh auth token returned empty")
    return token


def safe_error(operation: str, exc: BaseException) -> str:
    """Describe a failure without ever echoing exception text or credentials."""

    return f"{operation} failed ({type(exc).__name__})"


__all__ = ["BackendActionError", "HostRunner", "github_token", "safe_error", "subprocess_host_runner"]
