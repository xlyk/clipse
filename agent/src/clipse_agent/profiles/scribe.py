"""Scribe lane profile.

The Scribe lane runs on every merged Linear issue, inside a git worktree the
kernel has prepared on top of the latest merged code, and decides whether the
merge needs a documentation update -- writing one in its own PR, or making no
changes at all when there is nothing to write. This module is plain data,
mirroring `profiles.coder`'s and `profiles.reviewer`'s shape: it describes how
`dac.py` should build the lane's DAC agent
(`deepagents_code.agent.create_cli_agent`) -- it holds no live model client,
no secrets, and does no I/O.

Write-capable but narrowly scoped: unlike the Reviewer lane's read-mostly
list, this one can create/edit files and open its own PR (`git`/`gh`) -- but
unlike the Coder lane's, it carries none of the source-toolchain commands
(`go`/`uv`/`python`/`make`/`sed`/...): the Scribe lane only ever touches docs.
Per the DAC API spike findings (docs/design/2026-07-01-clipse-design.md),
shell enforcement of `shell_allow_list` still requires the agent to be built
with `auto_approve=False, interrupt_shell_only=True` -- that wiring lives in
`dac.py`, not here; this profile only carries the allow-list itself.

Always-on (design doc: "Runs on every merged issue; no-ops when there is
nothing to write."). From the `documentation` column, `internal/board.Next`
only allows `done`/`blocked` as outcomes, so a no-op is not a failure -- it is
the expected common case, not an edge case to prompt around defensively.
"""

from __future__ import annotations

from dataclasses import dataclass

_SYSTEM_PROMPT = """\
You are the Scribe lane of Clipse, a headless documentation agent. You run \
on every merged Linear issue, inside a git worktree the kernel has already \
checked out for you on top of the latest merged code.

- Stay inside the given worktree; do not touch other worktrees, branches, \
or repositories.
- Inspect the change that was just merged (for example `git log -1 --stat`, \
`git show`, or `git diff` against its parent commit) together with the \
repository's existing docs, and decide whether anything needs to be \
written or updated.
- You only write documentation. Never edit application or test source code \
-- that is the Coder lane's job, not yours.
- If the merged change is user- or contributor-facing and the docs don't \
already cover it, update or add the relevant documentation, matching the \
style and structure of the surrounding docs.
- If there is nothing worth documenting, make no file changes at all -- a \
no-op is a completely valid, expected outcome. Never invent busywork just \
to have something to commit.
- You do not need to run git or gh commands yourself to commit, push, or \
open a pull request -- the platform does that for you automatically \
whenever you leave file changes behind. Use git/gh only to inspect history \
or context.
- When you are done -- whether you wrote docs or a no-op is correct -- stop \
and report done. If you are missing information you cannot reasonably \
infer, stop and report blocked with a clear summary of what you need. Do \
not guess, and do not loop.
- Only run commands from your shell allow-list.
"""

# Write-capable (git/gh + mkdir for a new docs subdirectory), but no
# source-toolchain commands (no go/uv/python/make/sed/etc, unlike the Coder
# lane's list in profiles/coder.py) -- the Scribe lane only ever touches
# docs.
_SHELL_ALLOW_LIST: tuple[str, ...] = (
    "git",
    "gh",
    "ls",
    "cat",
    "grep",
    "rg",
    "find",
    "mkdir",
)


@dataclass(frozen=True)
class ScribeProfile:
    """DAC configuration for the Scribe lane.

    Fields line up with `profiles.coder.CoderProfile` / `profiles.reviewer.
    ReviewerProfile`: an assistant identity, a `provider:model` spec, a
    system prompt, and a shell allow-list. `shell_allow_list` is a tuple
    (not a list) so the frozen dataclass is actually immutable end to end.
    """

    assistant_id: str
    model: str
    system_prompt: str
    shell_allow_list: tuple[str, ...]


def get_scribe_profile() -> ScribeProfile:
    """Return the Scribe lane's DAC profile.

    `model` names a placeholder `provider:model` spec, never a live
    credential -- secrets (e.g. `ANTHROPIC_API_KEY`) reach the DAC agent via
    the worker's scrubbed environment, not this profile.
    """
    return ScribeProfile(
        assistant_id="clipse-scribe",
        model="anthropic:claude-sonnet-4-6",
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=_SHELL_ALLOW_LIST,
    )
