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


# ---------------------------------------------------------------------------
# Documentation sub-step profile
# ---------------------------------------------------------------------------
#
# The Coder lane runs a documentation turn (graphs/coder.py's `run_docs` node)
# right after the coding turn and before the PR is opened, so docs ride the
# same commit and same PR as the code. This is a docs-scoped profile for that
# turn: a docs-only system prompt plus a restricted allow-list. It deliberately
# omits the source-toolchain commands the coding turn has (no go/uv/python/
# make/sed/...), so the docs turn can only ever touch documentation. It reuses
# `CoderProfile` -- a docs turn is a sub-step of the Coder lane, not a separate
# lane -- so the graph's `AgentFactory` seam accepts it unchanged.
_DOCS_SYSTEM_PROMPT = """\
You are the documentation step of Clipse's Coder lane, a headless docs agent. \
You run right after the Coder lane finished editing THIS worktree for a single \
Linear issue -- the code change is still UNCOMMITTED in the working tree.

- Stay inside the given worktree; do not touch other worktrees, branches, or \
repositories.
- Inspect the change just made here. Because it is not committed yet, use \
`git status` (new/renamed files) and `git diff` (edits) -- NOT `git log` or \
`git show`, which won't show uncommitted work -- together with the \
repository's existing docs.
- You ONLY write documentation. Never edit application or test source code -- \
that is the coding step's job, not yours.
- If the change is user- or contributor-facing and the docs don't already \
cover it, update or add the relevant documentation, matching the style and \
structure of the surrounding docs.
- If nothing needs documenting, make NO file changes at all -- a no-op is a \
completely valid, expected outcome. Never invent busywork just to have \
something to commit.
- Do NOT run git or gh to commit, push, or open a pull request. The platform \
commits your documentation edits together with the code change, in the SAME \
commit and the SAME pull request. Use git only to inspect the uncommitted \
change and context.
- When you have written the docs (or decided none are needed), stop. Do not \
loop. Only run commands from your shell allow-list.
"""

# Docs-only: git/gh to inspect the uncommitted change + mkdir for a new docs
# subdirectory, but none of the source-toolchain commands in _SHELL_ALLOW_LIST
# above (no go/uv/python/make/sed) -- the docs turn only ever touches docs.
_DOCS_SHELL_ALLOW_LIST: tuple[str, ...] = (
    "git",
    "gh",
    "ls",
    "cat",
    "grep",
    "rg",
    "find",
    "mkdir",
)


def get_coder_docs_profile() -> CoderProfile:
    """Return the DAC profile for the Coder lane's documentation sub-step.

    A distinct `assistant_id` ("clipse-coder-docs") keeps the docs turn's
    telemetry/checkpoints separable from the coding turn's; the model matches
    the coding turn (docs need no stronger model). Like `get_coder_profile`,
    `model` is a placeholder spec, never a live credential.
    """
    return CoderProfile(
        assistant_id="clipse-coder-docs",
        model="anthropic:claude-sonnet-4-6",
        system_prompt=_DOCS_SYSTEM_PROMPT,
        shell_allow_list=_DOCS_SHELL_ALLOW_LIST,
    )
