"""Coder lane profile.

The Coder lane implements a Linear issue by editing files inside the git
worktree the kernel has already checked out; the kernel then commits, pushes,
and opens the PR deterministically (graphs/coder.py's commit/push/open_PR
nodes). This module is plain data: it describes how `dac.py` should build the
lane's DAC agent (`deepagents_code.agent.create_cli_agent`) — it holds no live
model client, no secrets, and does no I/O.

Per the DAC API spike findings (docs/design/2026-07-01-clipse-design.md),
shell enforcement of `shell_allow_list` requires the agent to be built with
`auto_approve=False, interrupt_shell_only=True` — `auto_approve=True`
silently disables the allow-list. That wiring lives in `dac.py`, not here;
this profile only carries the allow-list itself.
"""

from __future__ import annotations

from dataclasses import dataclass

_SYSTEM_PROMPT = """\
You are the Coder lane of Clipse, a headless coding agent. You implement a \
single Linear issue end-to-end inside the git worktree the kernel has \
already checked out for you.

- Stay inside the given worktree; do not touch other worktrees, branches, \
or repositories.
- Read the issue description before changing anything, and search the \
codebase for existing patterns to match.
- You do not need to run git or gh commands to commit, push, or open a pull \
request — the platform commits your work, pushes the branch, and opens the \
pull request for you automatically from the file changes you leave in the \
worktree. Use git/gh only to inspect history or context, never to commit, \
push, or open a PR yourself (attempting it will be rejected, and retrying \
in a loop only wastes your budget).
- Keep your changes focused: implement exactly what the issue asks, and do \
not bundle in unrelated edits.
- When the issue is fully implemented, stop and report done.
- If you are missing information you cannot reasonably infer — an \
ambiguous requirement, a missing credential, a decision only a human can \
make — stop and report blocked with a clear summary of what you need. Do \
not guess, and do not loop.
- Only run commands from your shell allow-list.
"""

_SHELL_ALLOW_LIST: tuple[str, ...] = (
    "git",
    "gh",
    "ls",
    "cat",
    "sed",
    "grep",
    "rg",
    "find",
    "mkdir",
    "go",
    "uv",
    "python",
    "python3",
    "make",
    "cd",
    "echo",
    "test",
)


@dataclass(frozen=True)
class CoderProfile:
    """DAC configuration for the Coder lane.

    Fields line up with the arguments `create_cli_agent`
    (`deepagents_code.agent`) needs to build the lane's DAC agent: an
    assistant identity, a `provider:model` spec, a system prompt, and a
    shell allow-list. `shell_allow_list` is a tuple (not a list) so the
    frozen dataclass is actually immutable end to end.
    """

    assistant_id: str
    model: str
    system_prompt: str
    shell_allow_list: tuple[str, ...]


def get_coder_profile() -> CoderProfile:
    """Return the Coder lane's DAC profile.

    `model` is a placeholder `provider:model` spec, never a live credential
    — secrets (e.g. `ANTHROPIC_API_KEY`) reach the DAC agent via the
    worker's scrubbed environment, not this profile.
    """
    return CoderProfile(
        assistant_id="clipse-coder",
        model="anthropic:claude-sonnet-4-6",
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=_SHELL_ALLOW_LIST,
    )
