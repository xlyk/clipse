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
from dataclasses import replace
from urllib.parse import quote

from daytona import Daytona, DaytonaConfig
from langchain_daytona import DaytonaSandbox

from clipse_agent import dac
from clipse_agent.backends.contracts import BackendActionRequest, BackendWorkspace
from clipse_agent.backends.daytona import (
    DaytonaLifecycle,
    DaytonaSession,
    RepositoryScopedDaytonaSandbox,
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


def wait_for_no_leftovers(
    list_workspaces: Callable[[], list[BackendWorkspace]],
    owner_fragment: str,
    *,
    attempts: int = 10,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    """Wait for Daytona's eventually consistent list to drop deleted objects."""

    for attempt in range(attempts):
        leftovers = [workspace for workspace in list_workspaces() if owner_fragment in workspace.owner_key]
        if not leftovers:
            return
        if attempt + 1 < attempts:
            sleep(1)
    raise SmokeError("Daytona smoke sandboxes remain")


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


async def run_agent_turn(*, profile: object, session: DaytonaSession, thread_id: str, task: str) -> str:
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
    turn = await dac.drive_turn(
        agent,
        {"configurable": {"thread_id": thread_id}},
        task_text=task,
        max_tokens=None,
    )
    if turn.outcome_hint != "completed" or turn.token_ceiling_exceeded:
        raise SmokeError(f"{profile.assistant_id} did not complete")
    return turn.last_text or turn.final_text


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

    created: list[tuple[BackendWorkspace, BackendActionRequest]] = []
    pr_url = ""
    branch_pushed = False
    cleanup_errors: list[str] = []
    try:
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
        coder_workspace = lifecycle.ensure(coder_request)
        created.append((coder_workspace, coder_request))
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
        branch_pushed = True

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
        reviewer_workspace = lifecycle.ensure(reviewer_request)
        created.append((reviewer_workspace, reviewer_request))
        print(f"reviewer sandbox: {reviewer_workspace.external_id}", flush=True)
        reviewer = attach(reviewer_workspace, slug)
        reviewer_model = os.getenv("CLIPSE_SMOKE_REVIEWER_MODEL", DEFAULT_REVIEWER_MODEL)
        verdict = await run_agent_turn(
            profile=get_reviewer_profile(model=reviewer_model),
            session=reviewer,
            thread_id=f"{issue_id}::reviewer",
            task=(
                "Review this authoritative GitHub pull-request diff. Read surrounding repository files only if needed. "
                "Do not edit, commit, push, or call gh. End with the profile's required verdict line.\n\n"
                f"{diff_result.stdout}"
            ),
        )
        if "VERDICT:" not in verdict:
            raise SmokeError("reviewer did not emit a verdict")
        print("verified: coder DAC, draft PR diff, and fresh reviewer DAC", flush=True)
    finally:
        if pr_url:
            try:
                run_host(["gh", "pr", "close", pr_url, "--delete-branch"])
                print("closed draft PR and deleted branch", flush=True)
            except Exception as exc:  # noqa: BLE001 - preserve remaining cleanup attempts
                cleanup_errors.append(f"PR cleanup failed ({type(exc).__name__})")
        elif branch_pushed:
            try:
                run_host(["gh", "api", "-X", "DELETE", f"repos/{slug}/git/refs/heads/{branch}"])
                print("deleted smoke branch", flush=True)
            except Exception as exc:  # noqa: BLE001 - preserve sandbox cleanup
                cleanup_errors.append(f"branch cleanup failed ({type(exc).__name__})")

        for workspace, request in reversed(created):
            try:
                lifecycle.delete(request.model_copy(update={"action": "delete", "sandbox_id": workspace.external_id}))
                print(f"deleted sandbox: {workspace.external_id}", flush=True)
            except Exception as exc:  # noqa: BLE001 - report type only
                cleanup_errors.append(f"workspace cleanup failed ({type(exc).__name__})")

        try:
            list_request = lifecycle_request(action="list", slug=slug)
            wait_for_no_leftovers(lambda: lifecycle.list(list_request), issue_id)
            print("final Daytona smoke leftovers: 0", flush=True)
        except Exception as exc:  # noqa: BLE001 - report type only
            cleanup_errors.append(f"Daytona cleanup query failed ({type(exc).__name__})")

        if branch_pushed:
            try:
                endpoint = f"repos/{slug}/git/matching-refs/heads/{quote(branch, safe='/')}"
                refs = json.loads(run_host(["gh", "api", endpoint]))
                print(f"final GitHub smoke branch refs: {len(refs)}", flush=True)
                if refs:
                    cleanup_errors.append("GitHub smoke branch remains")
            except Exception as exc:  # noqa: BLE001 - report type only
                cleanup_errors.append(f"GitHub cleanup query failed ({type(exc).__name__})")

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
