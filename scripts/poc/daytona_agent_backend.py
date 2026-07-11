#!/usr/bin/env python3
"""Live proof that DAC can use Daytona as its filesystem and shell backend."""

from __future__ import annotations

import argparse
import asyncio
import os
import re
import secrets
import shlex
import sys
from pathlib import Path

from daytona import CreateSandboxFromSnapshotParams, Daytona, DaytonaConfig
from deepagents_code.agent import create_cli_agent
from deepagents_code.config import create_model
from langchain_daytona import DaytonaSandbox

from clipse_agent.dac import drive_turn
from clipse_agent.backends.contracts import BackendActionError
from clipse_agent.backends.github import github_token, subprocess_host_runner

DEFAULT_MODEL = "anthropic:claude-sonnet-4-6"
REMOTE_REPO = "/home/daytona/workspace/clipse"
REMOTE_FILE = f"{REMOTE_REPO}/daytona-poc.txt"

POC_SYSTEM_PROMPT = f"""\
You are running a narrow Clipse isolation proof of concept. Your filesystem and
shell tools operate in a disposable Daytona sandbox, not on the host.

Work only in {REMOTE_REPO}. Do not commit, push, modify Git configuration, or
change any file except {REMOTE_FILE}. Follow the task exactly, verify it with
your tools, then stop with a short completion message.
"""


class PocError(RuntimeError):
    """A safe, operator-facing POC failure."""


def run_host(argv: list[str]) -> str:
    """Run a trusted host command without echoing stdout or stderr on failure."""
    try:
        return subprocess_host_runner(argv)
    except BackendActionError as exc:
        raise PocError(str(exc)) from None


def github_repo() -> tuple[str, str]:
    """Return the current origin as an HTTPS URL and its default branch."""
    remote = run_host(["git", "remote", "get-url", "origin"])
    match = re.fullmatch(r"git@github\.com:(?P<slug>[^/]+/[^/]+?)(?:\.git)?", remote)
    if match is None:
        match = re.fullmatch(
            r"https://github\.com/(?P<slug>[^/]+/[^/]+?)(?:\.git)?/?", remote
        )
    if match is None:
        raise PocError("origin must be a github.com SSH or HTTPS URL")

    slug = match.group("slug")
    branch = run_host(
        [
            "gh",
            "repo",
            "view",
            slug,
            "--json",
            "defaultBranchRef",
            "--jq",
            ".defaultBranchRef.name",
        ]
    )
    if not branch:
        raise PocError("GitHub returned an empty default branch")
    return f"https://github.com/{slug}.git", branch


def require_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise PocError(f"{name} is required; create a Daytona API key and export it")
    return value


def read_text(backend: DaytonaSandbox, path: str) -> str:
    result = backend.read(path)
    if result.error or result.file_data is None:
        raise PocError(f"cannot read remote file: {path}")
    if result.file_data["encoding"] != "utf-8":
        raise PocError(f"remote file is not UTF-8: {path}")
    return result.file_data["content"]


def execute_ok(backend: DaytonaSandbox, command: str, failure: str) -> str:
    result = backend.execute(command, timeout=30)
    if result.exit_code != 0:
        raise PocError(failure)
    return result.output


def assert_remote_state(backend: DaytonaSandbox, nonce: str, token: str) -> None:
    """Verify the remote file, shell, and credential boundary."""
    if read_text(backend, REMOTE_FILE) != nonce:
        raise PocError("remote nonce file does not match exactly")

    execute_ok(
        backend,
        f'test "$(cat {shlex.quote(REMOTE_FILE)})" = {shlex.quote(nonce)}',
        "independent remote shell assertion failed",
    )

    env_check = backend.execute("env | grep -E '^(GH_TOKEN|GITHUB_TOKEN)='", timeout=30)
    if env_check.exit_code == 0:
        raise PocError("GitHub token variable reached sandbox environment")

    git_config = read_text(backend, f"{REMOTE_REPO}/.git/config")
    if token in git_config:
        raise PocError("GitHub token reached sandbox Git config")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Run a disposable DAC + Daytona agent-backend proof of concept.",
        epilog=(
            "Requires DAYTONA_API_KEY, host gh authentication, and host model "
            "credentials. CLIPSE_POC_MODEL overrides the default model."
        ),
    )
    parser.add_argument(
        "--model",
        default=os.getenv("CLIPSE_POC_MODEL", DEFAULT_MODEL),
        help="provider:model spec (default: CLIPSE_POC_MODEL or Clipse Coder default)",
    )
    return parser


async def run_poc(model_spec: str) -> None:
    api_key = require_env("DAYTONA_API_KEY")
    clone_url, base_branch = github_repo()
    token = github_token()
    nonce = secrets.token_hex(16)

    local_probe = Path.cwd() / "daytona-poc.txt"
    if local_probe.exists():
        raise PocError(f"local probe path already exists: {local_probe}")

    config_kwargs: dict[str, str] = {"api_key": api_key}
    if api_url := os.getenv("DAYTONA_API_URL"):
        config_kwargs["api_url"] = api_url
    client = Daytona(DaytonaConfig(**config_kwargs))

    sandbox = None
    primary_error: BaseException | None = None
    try:
        sandbox = client.create(
            CreateSandboxFromSnapshotParams(
                labels={"created-by": "clipse-daytona-poc"},
                ephemeral=True,
                auto_stop_interval=10,
            )
        )
        print(f"Sandbox: {sandbox.id}", flush=True)

        sandbox.git.clone(
            url=clone_url,
            path="workspace/clipse",
            branch=base_branch,
            username="x-access-token",
            password=token,
        )

        backend = DaytonaSandbox(sandbox=sandbox)
        model = create_model(model_spec).model
        agent, _ = create_cli_agent(
            model,
            "clipse-daytona-poc",
            sandbox=backend,
            sandbox_type="daytona",
            system_prompt=POC_SYSTEM_PROMPT,
            interactive=False,
            auto_approve=True,
            enable_ask_user=False,
            enable_memory=False,
            enable_skills=False,
            enable_shell=True,
            checkpointer=None,
            cwd=REMOTE_REPO,
        )

        task = (
            f"Create {REMOTE_FILE} with exactly this content and no trailing newline: {nonce}\n"
            f"Use your file tools to read it back. Then use execute to verify the exact content. "
            "Do not change any other file."
        )
        turn = await drive_turn(
            agent,
            {"configurable": {"thread_id": f"daytona-poc-{nonce}"}},
            task_text=task,
            max_tokens=None,
        )
        if token in turn.final_text or token in turn.last_text:
            raise PocError("GitHub token appeared in agent output")

        assert_remote_state(backend, nonce, token)
        if local_probe.exists():
            raise PocError("agent wrote the POC file on the host")

        reattached = client.get(sandbox.id)
        second_backend = DaytonaSandbox(sandbox=reattached)
        assert_remote_state(second_backend, nonce, token)
        print("Verified: remote tools, credential boundary, and sandbox reattachment")
    except BaseException as exc:
        primary_error = exc
        raise
    finally:
        if sandbox is not None:
            try:
                client.delete(sandbox)
                print("Deleted sandbox")
            except Exception as cleanup_error:
                if primary_error is None:
                    raise PocError(
                        f"sandbox cleanup failed ({type(cleanup_error).__name__})"
                    ) from None
                print(
                    f"WARNING: sandbox cleanup failed ({type(cleanup_error).__name__}); "
                    f"delete sandbox {sandbox.id} manually",
                    file=sys.stderr,
                )


def main() -> int:
    args = build_parser().parse_args()
    try:
        asyncio.run(run_poc(args.model))
    except PocError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("FAIL: interrupted", file=sys.stderr)
        return 130
    except Exception as exc:
        print(f"FAIL: unexpected {type(exc).__name__}", file=sys.stderr)
        return 1

    print("PASS: Daytona agent backend POC")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
