#!/usr/bin/env python3
"""Exercise the production Daytona coder/reviewer path without merging."""

from __future__ import annotations

import asyncio
import json
import os
import secrets
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass, replace
from typing import Any
from urllib.parse import quote

from daytona import Daytona, DaytonaConfig
from langchain_daytona import DaytonaSandbox

from clipse_agent import dac
from clipse_agent.backends.contracts import BackendActionRequest, BackendWorkspace
from clipse_agent.backends.daytona import (
    DaytonaLifecycle,
    DaytonaSession,
    RepositoryScopedDaytonaSandbox,
    owner_key,
)
from clipse_agent.backends.github import (
    BackendActionError,
    subprocess_host_runner,
)
from clipse_agent.profiles.coder import get_coder_profile
from clipse_agent.profiles.reviewer import get_reviewer_profile

DEFAULT_CODER_MODEL = "anthropic:claude-sonnet-4-6"
DEFAULT_REVIEWER_MODEL = "anthropic:claude-opus-4-6"


class SmokeError(RuntimeError):
    """Sanitized failure from the opt-in live smoke."""


@dataclass(frozen=True)
class AgentTurnEvidence:
    final_text: str
    tool_calls: tuple[str, ...]


_REMOTE_TOOL_NAMES = frozenset(
    {"execute", "shell", "read_file", "write_file", "edit_file", "ls", "glob", "grep"}
)


def _open_pr_urls(run: Callable[[list[str]], str], slug: str, branch: str) -> list[str]:
    raw = run(
        ["gh", "pr", "list", "--repo", slug, "--state", "open", "--head", branch, "--json", "url"]
    )
    try:
        rows = json.loads(raw)
        return sorted(row["url"] for row in rows if isinstance(row, dict) and isinstance(row.get("url"), str))
    except (json.JSONDecodeError, TypeError):
        raise SmokeError("GitHub PR cleanup query returned invalid JSON") from None


def _branch_refs(run: Callable[[list[str]], str], slug: str, branch: str) -> list[str]:
    endpoint = f"repos/{slug}/git/matching-refs/heads/{quote(branch, safe='/')}"
    try:
        rows = json.loads(run(["gh", "api", endpoint]))
        return sorted(row["ref"] for row in rows if isinstance(row, dict) and isinstance(row.get("ref"), str))
    except (json.JSONDecodeError, TypeError):
        raise SmokeError("GitHub branch cleanup query returned invalid JSON") from None


def cleanup_github(
    run: Callable[[list[str]], str],
    slug: str,
    branch: str,
    *,
    attempts: int = 5,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    """Discover and remove live GitHub resources even after lost responses."""

    delete_endpoint = f"repos/{slug}/git/refs/heads/{quote(branch, safe='/')}"
    for attempt in range(attempts):
        try:
            for url in _open_pr_urls(run, slug, branch):
                try:
                    run(["gh", "pr", "close", url, "--repo", slug])
                except Exception:  # noqa: BLE001 - a lost response may still mean remote success
                    pass
        except Exception:  # noqa: BLE001 - retry boundedly, then verify authoritatively
            pass

        try:
            if _branch_refs(run, slug, branch):
                try:
                    run(["gh", "api", "-X", "DELETE", delete_endpoint])
                except Exception:  # noqa: BLE001 - verify the ref on the next observation
                    pass
        except Exception:  # noqa: BLE001 - retry boundedly, then verify authoritatively
            pass

        if attempt + 1 < attempts:
            sleep(1)

    try:
        prs = _open_pr_urls(run, slug, branch)
        refs = _branch_refs(run, slug, branch)
    except Exception as exc:  # noqa: BLE001 - include only safe resource identity and type
        raise SmokeError(f"GitHub cleanup could not verify {branch} ({type(exc).__name__})") from None
    if prs or refs:
        identities = sorted([*prs, *refs])
        raise SmokeError(f"GitHub cleanup incomplete for {branch}: {', '.join(identities)}")


def _delete_workspace(lifecycle: Any, request: BackendActionRequest, external_id: str) -> None:
    lifecycle.delete(request.model_copy(update={"action": "delete", "sandbox_id": external_id}))


def cleanup_sandboxes(
    lifecycle: Any,
    list_request: BackendActionRequest,
    requests: list[BackendActionRequest],
    owner_fragment: str,
    *,
    known: list[tuple[BackendWorkspace, BackendActionRequest]],
    attempts: int = 10,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    """Rediscover labeled sandboxes and retry delete by external ID."""

    requests_by_owner = {owner_key(request): request for request in requests}
    observed_ids = {workspace.external_id for workspace, _ in known}
    for workspace, request in known:
        try:
            _delete_workspace(lifecycle, request, workspace.external_id)
        except Exception:  # noqa: BLE001 - list is the authoritative cleanup verification
            pass

    for attempt in range(attempts):
        try:
            workspaces = lifecycle.list(list_request)
        except Exception:  # noqa: BLE001 - retry boundedly, then surface only known IDs
            workspaces = []
        for workspace in workspaces:
            if owner_fragment not in workspace.owner_key:
                continue
            observed_ids.add(workspace.external_id)
            request = requests_by_owner.get(workspace.owner_key)
            if request is None:
                continue
            try:
                _delete_workspace(lifecycle, request, workspace.external_id)
            except Exception:  # noqa: BLE001 - a lost response may still mean remote success
                pass
        if attempt + 1 < attempts:
            sleep(1)

    try:
        leftovers = [
            workspace for workspace in lifecycle.list(list_request) if owner_fragment in workspace.owner_key
        ]
    except Exception as exc:  # noqa: BLE001 - never forward provider response bodies
        ids = ", ".join(sorted(observed_ids)) or "unknown"
        raise SmokeError(f"Daytona cleanup could not verify sandbox IDs {ids} ({type(exc).__name__})") from None
    if leftovers:
        ids = ", ".join(sorted(workspace.external_id for workspace in leftovers))
        raise SmokeError(f"Daytona cleanup incomplete; delete sandbox IDs manually: {ids}")


def run_host(argv: list[str]) -> str:
    try:
        return subprocess_host_runner(argv)
    except BackendActionError as exc:
        raise SmokeError(str(exc)) from None


def require_environment() -> None:
    if not os.getenv("DAYTONA_API_KEY"):
        raise SmokeError("DAYTONA_API_KEY is required")
    run_host(["gh", "auth", "status", "--hostname", "github.com"])


def repository_identity() -> tuple[str, str, str]:
    slug = run_host(["gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner"])
    base = run_host(
        ["gh", "repo", "view", slug, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name"]
    )
    if not slug or not base:
        raise SmokeError("GitHub returned an incomplete repository identity")
    return slug, f"https://github.com/{slug}.git", base


def lifecycle_request(
    *,
    action: str,
    slug: str,
    repo_url: str | None = None,
    base: str | None = None,
    branch: str | None = None,
    issue_id: str | None = None,
    run_id: str | None = None,
    role: str | None = None,
    sandbox_id: str | None = None,
) -> BackendActionRequest:
    return BackendActionRequest(
        action=action,
        provider="daytona",
        repo_slug=slug,
        repo_url=repo_url,
        base_branch=base,
        branch=branch,
        issue_id=issue_id,
        run_id=run_id,
        role=role,
        sandbox_id=sandbox_id,
        auto_stop_minutes=60 if action == "ensure" else None,
        reviewer_auto_delete_minutes=60 if action == "ensure" else None,
        snapshot=os.getenv("CLIPSE_DAYTONA_SNAPSHOT") or None,
        target=os.getenv("CLIPSE_DAYTONA_TARGET") or None,
    )


def attach(workspace: BackendWorkspace, slug: str) -> DaytonaSession:
    target = os.getenv("CLIPSE_DAYTONA_TARGET") or None
    sdk = Daytona(DaytonaConfig(target=target)).get(workspace.external_id)
    remote = RepositoryScopedDaytonaSandbox(DaytonaSandbox(sandbox=sdk), workspace.workspace_path)
    return DaytonaSession(workspace.workspace_path, slug, remote, sdk)


def require_command_ok(label: str, result: object) -> None:
    if getattr(result, "returncode", 1) != 0:
        raise SmokeError(f"{label} failed")


def verify_reviewer_remote_state(
    session: DaytonaSession,
    branch: str,
    marker_path: str,
    marker: str,
) -> None:
    branch_result = session.run(["git", "branch", "--show-current"])
    require_command_ok("reviewer branch verification", branch_result)
    if branch_result.stdout.strip() != branch:
        raise SmokeError("reviewer sandbox checked out the wrong branch")
    marker_result = session.run(["cat", marker_path])
    require_command_ok("reviewer marker verification", marker_result)
    if marker_result.stdout.strip() != marker:
        raise SmokeError("reviewer sandbox marker did not match")


def validate_reviewer_evidence(evidence: AgentTurnEvidence) -> str:
    if not _REMOTE_TOOL_NAMES.intersection(evidence.tool_calls):
        raise SmokeError("reviewer did not use a Daytona filesystem or shell tool")
    verdicts = []
    for line in evidence.final_text.splitlines():
        if line == "VERDICT: PASS":
            verdicts.append("PASS")
        elif line == "VERDICT: CHANGES_REQUESTED":
            verdicts.append("CHANGES_REQUESTED")
    if len(verdicts) != 1:
        raise SmokeError("reviewer must emit exactly one allowed verdict line")
    return verdicts[0]


async def run_agent_turn(
    *,
    profile: object,
    session: DaytonaSession,
    thread_id: str,
    task: str,
) -> AgentTurnEvidence:
    profile = replace(
        profile,
        system_prompt=(
            f"{profile.system_prompt}\n\n"
            f"The absolute repository root in this Daytona sandbox is {session.cwd}. "
            "Treat it as the workspace root for every repository operation."
        ),
    )
    agent, _ = dac.build_coder_agent(
        profile,
        None,
        session.cwd,
        sandbox=session.sandbox,
        sandbox_type=session.sandbox_type,
    )
    tool_calls: list[str] = []

    def evidence_sink(event: dict[str, Any]) -> None:
        if event.get("event") == "tool_call" and isinstance(event.get("name"), str):
            tool_calls.append(event["name"])

    turn = await dac.drive_turn(
        agent,
        {"configurable": {"thread_id": thread_id}},
        task_text=task,
        max_tokens=None,
        event_sink=evidence_sink,
    )
    if turn.outcome_hint != "completed" or turn.token_ceiling_exceeded:
        raise SmokeError(f"{profile.assistant_id} did not complete")
    return AgentTurnEvidence(turn.last_text or turn.final_text, tuple(tool_calls))


async def run_smoke() -> None:
    require_environment()
    slug, repo_url, base = repository_identity()
    nonce = secrets.token_hex(5)
    issue_id = f"smoke-daytona-{nonce}"
    reviewer_run_id = f"smoke-reviewer-{nonce}"
    branch = f"smoke/daytona-{nonce}"
    marker_path = f"daytona-smoke-{nonce}.txt"
    marker = f"clipse Daytona smoke {nonce}"
    title = f"test: daytona backend smoke {nonce}"
    lifecycle = DaytonaLifecycle()
    coder_request = lifecycle_request(
        action="ensure",
        slug=slug,
        repo_url=repo_url,
        base=base,
        branch=branch,
        issue_id=issue_id,
        run_id=f"coder-{nonce}",
        role="coder",
    )
    reviewer_request = lifecycle_request(
        action="ensure",
        slug=slug,
        repo_url=repo_url,
        base=base,
        branch=branch,
        issue_id=issue_id,
        run_id=reviewer_run_id,
        role="reviewer",
    )
    list_request = lifecycle_request(action="list", slug=slug)
    known: list[tuple[BackendWorkspace, BackendActionRequest]] = []

    try:
        coder_workspace = lifecycle.ensure(coder_request)
        known.append((coder_workspace, coder_request))
        print(f"coder sandbox: {coder_workspace.external_id}", flush=True)
        coder = attach(coder_workspace, slug)
        require_command_ok("feature branch checkout", coder.run(["git", "checkout", "-b", branch]))

        coder_model = os.getenv("CLIPSE_SMOKE_CODER_MODEL", DEFAULT_CODER_MODEL)
        await run_agent_turn(
            profile=get_coder_profile(model=coder_model),
            session=coder,
            thread_id=f"{issue_id}::coder",
            task=(
                f"Create only {marker_path} with exactly this one line: {marker}\n"
                "Read it back with your tools, confirm the content, and stop. Do not run git or gh."
            ),
        )
        readback = coder.run(["cat", marker_path])
        require_command_ok("remote marker read", readback)
        if readback.stdout.strip() != marker:
            raise SmokeError("coder marker content did not match")
        require_command_ok("Daytona SDK commit", coder.commit(title))
        require_command_ok("Daytona SDK push", coder.push(branch))

        created_pr = coder.github(
            [
                "gh",
                "pr",
                "create",
                "--head",
                branch,
                "--base",
                base,
                "--draft",
                "--title",
                title,
                "--body",
                "Automated Daytona backend smoke. This draft PR is closed by the smoke and is never merged.",
            ]
        )
        require_command_ok("draft PR creation", created_pr)
        pr_url = created_pr.stdout.strip()
        if not pr_url.startswith("https://github.com/"):
            raise SmokeError("GitHub returned an invalid PR URL")
        print(f"draft PR: {pr_url}", flush=True)

        diff_result = coder.github(["gh", "pr", "diff", pr_url])
        require_command_ok("authoritative PR diff", diff_result)
        if marker_path not in diff_result.stdout or marker not in diff_result.stdout:
            raise SmokeError("authoritative PR diff did not contain the marker change")

        reviewer_workspace = lifecycle.ensure(reviewer_request)
        known.append((reviewer_workspace, reviewer_request))
        print(f"reviewer sandbox: {reviewer_workspace.external_id}", flush=True)
        if reviewer_workspace.external_id == coder_workspace.external_id:
            raise SmokeError("reviewer did not receive a fresh sandbox")
        reviewer = attach(reviewer_workspace, slug)
        verify_reviewer_remote_state(reviewer, branch, marker_path, marker)
        reviewer_model = os.getenv("CLIPSE_SMOKE_REVIEWER_MODEL", DEFAULT_REVIEWER_MODEL)
        evidence = await run_agent_turn(
            profile=get_reviewer_profile(model=reviewer_model),
            session=reviewer,
            thread_id=f"{issue_id}::reviewer",
            task=(
                f"Review this authoritative GitHub pull-request diff. Before deciding, use the shell tool to run "
                f"`git branch --show-current` and `cat {marker_path}` in the Daytona repository; verify the branch "
                f"is `{branch}` and the marker is `{marker}`. Do not edit, commit, push, or call gh. End with the "
                "profile's required verdict line.\n\n"
                f"{diff_result.stdout}"
            ),
        )
        verdict = validate_reviewer_evidence(evidence)
        print(f"verified: coder DAC, draft PR diff, and fresh reviewer DAC ({verdict})", flush=True)
    finally:
        cleanup_errors: list[str] = []
        try:
            cleanup_github(run_host, slug, branch)
            print("final GitHub open PRs: 0", flush=True)
            print("final GitHub smoke branch refs: 0", flush=True)
        except Exception as exc:  # noqa: BLE001 - report type only
            cleanup_errors.append(str(exc) if isinstance(exc, SmokeError) else f"GitHub cleanup failed ({type(exc).__name__})")
        try:
            cleanup_sandboxes(
                lifecycle,
                list_request,
                [coder_request, reviewer_request],
                issue_id,
                known=known,
            )
            print("final Daytona smoke leftovers: 0", flush=True)
        except Exception as exc:  # noqa: BLE001 - preserve GitHub cleanup result
            cleanup_errors.append(
                str(exc) if isinstance(exc, SmokeError) else f"Daytona cleanup failed ({type(exc).__name__})"
            )

        if cleanup_errors:
            raise SmokeError("; ".join(cleanup_errors))


def main() -> int:
    try:
        asyncio.run(run_smoke())
    except (SmokeError, BackendActionError) as exc:
        print(f"smoke failed: {exc}", file=sys.stderr)
        return 1
    except Exception as exc:  # noqa: BLE001 - never print provider/model exception text
        print(f"smoke failed ({type(exc).__name__})", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
