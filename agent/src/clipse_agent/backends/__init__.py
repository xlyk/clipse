"""Remote execution backend lifecycle primitives."""

from clipse_agent.backends.contracts import (
    BackendActionError,
    BackendActionRequest,
    BackendActionResult,
    BackendWorkspace,
)
from clipse_agent.backends.daytona import DaytonaLifecycle
from clipse_agent.backends.daytona import DaytonaSession, RepositoryScopedDaytonaSandbox
from clipse_agent.backends.local import LocalSession
from clipse_agent.backends.session import AgentSession, CommandResult

__all__ = [
    "BackendActionError",
    "BackendActionRequest",
    "BackendActionResult",
    "BackendWorkspace",
    "DaytonaLifecycle",
    "DaytonaSession",
    "RepositoryScopedDaytonaSandbox",
    "LocalSession",
    "AgentSession",
    "CommandResult",
]
