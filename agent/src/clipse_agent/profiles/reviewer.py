"""Reviewer lane profile.

The Reviewer lane checks out the Coder lane's PR branch -- the same
worktree the kernel already prepared, reused as-is -- and reviews the diff:
either it passes, or it requests changes with specific inline comments. This
module is plain data, mirroring `profiles.coder`'s shape: it describes how
`dac.py` should build the lane's DAC agent
(`deepagents_code.agent.create_cli_agent`) -- it holds no live model client,
no secrets, and does no I/O.

Read-mostly by design: the allow-list has no destructive or write-capable
commands (no `sed`, `mkdir`, `go`, `uv`, `python`, `make`, etc, unlike the
Coder lane's) -- a reviewer inspects, it never edits, stages, commits, or
pushes. Per the DAC API spike findings
(docs/design/2026-07-01-clipse-design.md), shell enforcement of
`shell_allow_list` still requires the agent to be built with
`auto_approve=False, interrupt_shell_only=True` -- that wiring lives in
`dac.py`, not here; this profile only carries the allow-list itself.

The reviewer's verdict is advisory only (design doc: "reviewer pass is
advisory input, never sufficient alone" -- the authoritative merge gate is
CI + branch protection, enforced by `internal/gitops`, not this lane).
Coder and Reviewer sharing one model family would make a reviewer approving
its own sibling's code a weaker safety signal, so `model` here is free to
name a distinct or stronger model than the Coder lane's -- see
`get_reviewer_profile`.
"""

from __future__ import annotations

from dataclasses import dataclass

_SYSTEM_PROMPT = """\
You are the Reviewer lane of Clipse, a headless code-review agent. You \
review a single Linear issue's pull request inside the git worktree the \
kernel has already checked out for you -- the same worktree and branch the \
Coder lane used.

- Stay inside the given worktree; do not touch other worktrees, branches, \
or repositories.
- You are read-mostly. Only run commands from your shell allow-list, and \
never attempt to edit, stage, commit, or push anything -- that is the \
Coder lane's job, not yours.
- The PR diff (base...HEAD) is included for you in the task text below -- \
review it for correctness, quality, and whether it satisfies the issue's \
requirements. Read surrounding files with `cat`, `grep`, `rg`, or `find` \
for context whenever the diff alone isn't enough.
- When you are done, end your final message with exactly one verdict line: \
`VERDICT: PASS` if the change is correct and ready to merge, or `VERDICT: \
CHANGES_REQUESTED` if it is not.
- If you request changes, list every finding as its own bullet line \
immediately below the verdict, in exactly this form: `- path/to/file.py:LINE: \
comment text` (one file:line per finding), so each becomes its own inline \
PR comment.
- Your verdict is advisory: it never merges or blocks anything by itself, \
and it is never sufficient on its own to land the change -- required CI \
checks and branch protection are the actual merge gate. Be specific and \
actionable in every comment.
- Do not guess about requirements you cannot verify from the issue text and \
the diff. If something is genuinely ambiguous, say so in a comment rather \
than assuming either verdict, and prefer CHANGES_REQUESTED over an unearned \
PASS.
"""

# Read-mostly: no write/execute-capable commands (no sed/mkdir/go/uv/python/
# make/echo/etc, unlike the Coder lane's list in profiles/coder.py) -- a
# reviewer inspects the code, it never edits it.
_SHELL_ALLOW_LIST: tuple[str, ...] = (
    "git",
    "gh",
    "cat",
    "ls",
    "grep",
    "rg",
    "find",
)


@dataclass(frozen=True)
class ReviewerProfile:
    """DAC configuration for the Reviewer lane.

    Fields line up with `profiles.coder.CoderProfile`: an assistant
    identity, a `provider:model` spec, a system prompt, and a shell
    allow-list. `shell_allow_list` is a tuple (not a list) so the frozen
    dataclass is actually immutable end to end.
    """

    assistant_id: str
    model: str
    system_prompt: str
    shell_allow_list: tuple[str, ...]


def get_reviewer_profile() -> ReviewerProfile:
    """Return the Reviewer lane's DAC profile.

    `model` names a placeholder `provider:model` spec, never a live
    credential -- secrets (e.g. `ANTHROPIC_API_KEY`) reach the DAC agent via
    the worker's scrubbed environment, not this profile. It is deliberately
    a *different* model than `profiles.coder.get_coder_profile`'s: the
    design doc calls for "a stronger or distinct model to reduce correlated
    blind spots" precisely because a reviewer sharing the coder's model
    family is advisory signal, not a safety guarantee.
    """
    return ReviewerProfile(
        assistant_id="clipse-reviewer",
        model="anthropic:claude-opus-4-6",
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=_SHELL_ALLOW_LIST,
    )
