"""Coder lane profile.

The Coder lane edits files, commits, pushes, and opens the PR for a Linear
issue inside the git worktree the kernel has already checked out. This
module is plain data: it describes how `dac.py` should build the lane's DAC
agent (`deepagents_code.agent.create_cli_agent`) — it holds no live model
client, no secrets, and does no I/O.

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
- Make focused, atomic commits as you go: one logical change per commit, \
with a clear message. Do not bundle unrelated changes together.
- Push your branch and open a pull request that references the issue once \
the implementation is ready for review.
- When the issue is fully implemented and committed, stop and report done.
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
