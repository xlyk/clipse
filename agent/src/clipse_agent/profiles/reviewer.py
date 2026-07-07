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
(docs/design/2026-07-01-clipse-design.md) and the per-lane shell-policy
decision (2026-07-07, see AGENTS.md), there are two sanctioned
`shell_allow_list` modes: a tuple is the restrictive mode and still requires
the agent to be built with `auto_approve=False, interrupt_shell_only=True`;
`None` is the unrestricted mode (DAC `auto_approve=True`, no allow-list) and
is the default from `get_reviewer_profile` below. That wiring lives in
`dac.py`, not here; this profile only carries the policy itself.

The reviewer's verdict is advisory only (design doc: "reviewer pass is
advisory input, never sufficient alone" -- the authoritative merge gate is
CI + branch protection, enforced by `internal/gitops`, not this lane).
Coder and Reviewer sharing one model family would make a reviewer approving
its own sibling's code a weaker safety signal, so `model` here is free to
name a distinct or stronger model than the Coder lane's -- see
`get_reviewer_profile`.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any

_SYSTEM_PROMPT = """\
You are the Reviewer lane of Clipse, a headless code-review agent. You \
review a single Linear issue's pull request inside the git worktree the \
kernel has already checked out for you -- the same worktree and branch the \
Coder lane used.

- Stay inside the given worktree; do not touch other worktrees, branches, \
or repositories.
- You are read-mostly, and never attempt to edit, stage, commit, or push \
anything -- that is the Coder lane's job, not yours. If a shell command is \
rejected, do not retry it in a loop -- try another approach or report \
blocked.
- The PR diff (base...HEAD) is included for you in the task text below -- \
review it for correctness, quality, and whether it satisfies the issue's \
requirements. Read surrounding files with `cat`, `grep`, `rg`, or `find` \
for context whenever the diff alone isn't enough.
- Never `cat` or otherwise read the contents of binary or image files (for \
example `.png`, `.jpg`, `.jpeg`, `.gif`, `.bmp`, `.tiff`, `.ico`, `.webp`, \
`.pdf`, or any other non-text asset). Reading one attaches it to the model \
as an image the review API cannot process, which aborts the entire review. \
Such files appear in the diff as `Binary files ... differ`; judge them only \
by their path, size, and stated purpose, never by their raw contents.
- When you are done, end your final message with exactly one verdict line: \
`VERDICT: PASS` if the change is correct and ready to merge, or `VERDICT: \
CHANGES_REQUESTED` if it is not.
- List every finding as its own bullet line immediately below the verdict, \
in exactly this form: `- blocking: path/to/file.py:LINE: comment text` for a \
defect that must be fixed before merge, or `- nit: path/to/file.py:LINE: \
comment text` for polish (one file:line per finding), so each becomes its \
own inline PR comment. Only blocking findings justify VERDICT: \
CHANGES_REQUESTED; a review whose findings are all nits should PASS.
- Never comment on formatting or whitespace in generated files \
(project.pbxproj, *.gen.go, *_generated.*, package lockfiles).
- Before emitting a verdict, enumerate EVERY instance of each defect class \
you report (grep for the pattern); a second review round must never be \
needed for the same class.
- Your verdict is advisory: it never merges or blocks anything by itself, \
and it is never sufficient on its own to land the change -- required CI \
checks and branch protection are the actual merge gate. Be specific and \
actionable in every comment.
- Do not guess about requirements you cannot verify from the issue text and \
the diff. If something is genuinely ambiguous, say so in a comment rather \
than assuming either verdict, and prefer CHANGES_REQUESTED over an unearned \
PASS.
"""

_DEFAULT_MODEL = "anthropic:claude-opus-4-6"

# Mirrors profiles.coder's own constant of the same name/value: lowers the
# trigger DAC's already-installed auto-summarizer uses (see
# dac.build_coder_agent) well below a big-context-window model's real limit.
_DEFAULT_CONTEXT_WINDOW_TOKENS = 200_000

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
    identity, a `provider:model` spec, a system prompt, and a shell policy.
    `shell_allow_list` is `None` or a tuple, never a bare `list`, mirroring
    `CoderProfile.shell_allow_list`'s own two sanctioned modes: `None` means
    unrestricted (the default from `get_reviewer_profile` below, decision
    2026-07-07); a tuple means the restrictive mode and stays a tuple (not a
    list) so the frozen dataclass is actually immutable end to end when it
    is set. `model_params` is a plain `dict`, mirroring
    `CoderProfile.model_params` -- frozen blocks *reassigning* the field,
    not mutating the dict it points to, and this value is only ever read
    after being built once.

    `context_window_tokens` mirrors `CoderProfile.context_window_tokens`:
    when not `None`, `dac.build_coder_agent` (reused unchanged for this lane
    -- see `get_reviewer_profile`) writes it onto the built model's own
    `profile["max_input_tokens"]` before `create_cli_agent`, lowering the
    trigger DAC's already-installed auto-summarizer uses.
    """

    assistant_id: str
    model: str
    system_prompt: str
    shell_allow_list: tuple[str, ...] | None
    model_params: dict[str, Any] | None = None
    context_window_tokens: int | None = _DEFAULT_CONTEXT_WINDOW_TOKENS


def get_reviewer_profile(
    model: str | None = None,
    model_params: dict[str, Any] | None = None,
    context_window_tokens: int | None = None,
    shell_allow_list: Sequence[str] | None = None,
) -> ReviewerProfile:
    """Return the Reviewer lane's DAC profile.

    `model` names a placeholder `provider:model` spec, never a live
    credential -- secrets (e.g. `ANTHROPIC_API_KEY`) reach the DAC agent via
    the worker's scrubbed environment, not this profile. When omitted
    (`None`), falls back to `_DEFAULT_MODEL`, which is deliberately a
    *different* model than `profiles.coder.get_coder_profile`'s default: the
    design doc calls for "a stronger or distinct model to reduce correlated
    blind spots" precisely because a reviewer sharing the coder's model
    family is advisory signal, not a safety guarantee.

    `model_params` is an opaque bag of extra model-construction kwargs
    (config.ModelParams's `Reviewer` map, threaded through as JSON via
    `worker.py`'s `--model-params` flag). Unlike `model`, it has no default
    to fall back to -- omitted (`None`) means exactly that: no overrides.

    `context_window_tokens` mirrors `model`'s idiom: omitted (`None`) falls
    back to `_DEFAULT_CONTEXT_WINDOW_TOKENS`, so this factory can never
    produce a profile with `context_window_tokens=None` -- only a
    directly-constructed `ReviewerProfile` can opt all the way out.

    `shell_allow_list` mirrors `profiles.coder.get_coder_profile`'s own
    odd-one-out idiom: omitted (`None`) means exactly `None` -- unrestricted,
    the new default (decision 2026-07-07) -- never a fall back to
    `_SHELL_ALLOW_LIST`. `worker.py` resolves this from the kernel's
    `--shell-allow-list` flag (absent -> `all` -> `None`).
    """
    return ReviewerProfile(
        assistant_id="clipse-reviewer",
        model=model if model is not None else _DEFAULT_MODEL,
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=tuple(shell_allow_list) if shell_allow_list is not None else None,
        model_params=model_params,
        context_window_tokens=(
            context_window_tokens if context_window_tokens is not None else _DEFAULT_CONTEXT_WINDOW_TOKENS
        ),
    )
