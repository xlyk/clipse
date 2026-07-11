"""Tests for the host-local runtime session."""

from __future__ import annotations

from types import SimpleNamespace

from clipse_agent.backends import local
from clipse_agent.backends.local import LocalSession
from clipse_agent.backends.session import AgentSession, CommandResult
from clipse_agent.graphs import coder, reviewer


def test_local_session_run_preserves_subprocess_semantics(monkeypatch) -> None:
    calls: list[tuple[list[str], dict[str, object]]] = []

    def fake_run(argv: list[str], **kwargs: object) -> SimpleNamespace:
        calls.append((argv, kwargs))
        return SimpleNamespace(returncode=7, stdout="stdout", stderr="stderr")

    monkeypatch.setattr(local.subprocess, "run", fake_run)
    session = LocalSession("/work/issue-1", "xlyk/clipse")

    result = session.run(["git", "status", "--short"])

    assert result == CommandResult(returncode=7, stdout="stdout", stderr="stderr")
    assert calls == [
        (
            ["git", "status", "--short"],
            {"cwd": "/work/issue-1", "capture_output": True, "text": True},
        )
    ]
    assert session.provider == "local"
    assert session.sandbox is None
    assert session.sandbox_type is None


def test_graphs_share_the_backend_command_result_type() -> None:
    assert coder.CommandResult is CommandResult
    assert reviewer.CommandResult is CommandResult


def test_local_session_github_owns_the_gh_executable(monkeypatch) -> None:
    calls: list[list[str]] = []

    def fake_run(argv: list[str], **_kwargs: object) -> SimpleNamespace:
        calls.append(argv)
        return SimpleNamespace(returncode=0, stdout="ok", stderr="")

    monkeypatch.setattr(local.subprocess, "run", fake_run)
    session = LocalSession("/work/issue-1", "xlyk/clipse")

    assert session.github(["pr", "view", "feat/CLI-1"]) == CommandResult(0, "ok")
    assert calls == [["gh", "pr", "view", "feat/CLI-1"]]


def test_agent_session_protocol_declares_merge_completion() -> None:
    assert callable(AgentSession.__dict__["commit_merge"])


def test_local_session_commit_merge_preserves_existing_merge_message(monkeypatch) -> None:
    calls: list[list[str]] = []

    def fake_run(argv: list[str], **_kwargs: object) -> SimpleNamespace:
        calls.append(argv)
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setattr(local.subprocess, "run", fake_run)
    session = LocalSession("/work/issue-1", "xlyk/clipse")

    assert session.commit_merge() == CommandResult(0)
    assert calls == [["git", "commit", "--no-edit"]]
