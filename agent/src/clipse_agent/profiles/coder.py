"""Coder lane profile.

The Coder lane implements a Linear issue by editing files inside the git
worktree the kernel has already checked out; the kernel then commits, pushes,
and opens the PR deterministically (graphs/coder.py's commit/push/open_PR
nodes). This module is plain data: it describes how `dac.py` should build the
lane's DAC agent (`deepagents_code.agent.create_cli_agent`) — it holds no live
model client, no secrets, and does no I/O.

Per the DAC API spike findings (docs/design/2026-07-01-clipse-design.md) and
the per-lane shell-policy decision (2026-07-07, see AGENTS.md), there are two
sanctioned `shell_allow_list` modes. A tuple is the restrictive mode: the
agent must be built with `auto_approve=False, interrupt_shell_only=True` —
`auto_approve=True` silently disables the allow-list. `None` is the
unrestricted mode — DAC `auto_approve=True`, no allow-list at all — and is
the default from the factories below, matching the kernel's own `all`-policy
default (`internal/config`'s `shell_allow_list`). That wiring lives in
`dac.py`, not here; this profile only carries the policy itself.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass
from typing import Any

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
push, or open a PR yourself (the platform performs these itself from your \
worktree changes; doing it yourself creates duplicate or non-draft PRs — \
never do it, and never retry a failing command in a loop).
- Keep your changes focused: implement exactly what the issue asks, and do \
not bundle in unrelated edits.
- When the issue is fully implemented, stop and report done.
- If you are missing information you cannot reasonably infer — an \
ambiguous requirement, a missing credential, a decision only a human can \
make — stop and report blocked with a clear summary of what you need. Do \
not guess, and do not loop.
- If your context grows large, call the `compact_conversation` tool to \
summarize older history before continuing.
- If a shell command is rejected, do not retry it in a loop — try another \
approach or report blocked.
- End your FINAL message with this exact tail (own lines, in this order):
  STATUS: done            (or: STATUS: blocked: <what you need and why>)
  TITLE: <lowercase conventional-commit line for this change, <=60 chars>
  HANDOFF:
  <3-8 bullet lines for the next agent: decisions made, interfaces
  added (exact names/signatures), deviations from the issue text,
  gotchas for dependent work, what was intentionally NOT done>
"""

# Single-sourced so get_coder_profile and get_coder_docs_profile -- which
# share the same default model -- can't drift apart.
_DEFAULT_MODEL = "anthropic:claude-sonnet-4-6"

# Lowers the trigger DAC's already-installed auto-summarizer uses (it fires
# at ~85% of model.profile["max_input_tokens"] per round -- see
# dac.build_coder_agent) well below a big-context-window model's real limit,
# so a long-running turn compacts its own history instead of ballooning past
# a fixed per-round ceiling. Single-sourced for the same reason as
# _DEFAULT_MODEL above.
_DEFAULT_CONTEXT_WINDOW_TOKENS = 200_000

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
    shell policy. `shell_allow_list` is `None` or a tuple, never a bare
    `list`: `None` means unrestricted — `dac.build_coder_agent` builds the
    agent with `auto_approve=True` and no allow-list at all (decision
    2026-07-07, the default from the factories below); a tuple means the
    restrictive mode — `auto_approve=False, interrupt_shell_only=True`,
    DAC's own allow-list enforcement — kept as a tuple (not a list) so the
    frozen dataclass stays immutable end to end when it is set.
    `model_params` is a plain `dict` rather than a tuple: frozen blocks
    *reassigning* the field, not mutating the dict it points to, and unlike
    `shell_allow_list` this value is never mutated in place — it is built
    once (config.ModelParams, threaded through the kernel and `worker.py`'s
    `--model-params` JSON) and only ever read.

    `context_window_tokens`, when not `None`, is the ceiling
    `dac.build_coder_agent` writes onto the built model's own
    `profile["max_input_tokens"]` before handing it to `create_cli_agent` --
    lowering the trigger DAC's already-installed auto-summarizer
    (`SummarizationMiddleware`) uses, which is a fraction (0.85) of that same
    profile value per round. Defaults on (`_DEFAULT_CONTEXT_WINDOW_TOKENS`)
    so every lane gets this for free; `None` opts a hand-built profile out
    entirely, leaving `create_cli_agent` to see the model's own real (and
    possibly much larger) advertised window.
    """

    assistant_id: str
    model: str
    system_prompt: str
    shell_allow_list: tuple[str, ...] | None
    model_params: dict[str, Any] | None = None
    context_window_tokens: int | None = _DEFAULT_CONTEXT_WINDOW_TOKENS


def get_coder_profile(
    model: str | None = None,
    model_params: dict[str, Any] | None = None,
    context_window_tokens: int | None = None,
    shell_allow_list: Sequence[str] | None = None,
) -> CoderProfile:
    """Return the Coder lane's DAC profile.

    `model` is a placeholder `provider:model` spec, never a live credential
    — secrets (e.g. `ANTHROPIC_API_KEY`) reach the DAC agent via the
    worker's scrubbed environment, not this profile. When omitted (`None`),
    falls back to `_DEFAULT_MODEL`; `worker.py` passes an explicit override
    resolved from the kernel's `--model` flag.

    `model_params` is an opaque bag of extra model-construction kwargs
    (config.ModelParams's `Coder` map, threaded through as JSON via
    `worker.py`'s `--model-params` flag). Unlike `model`, it has no default
    to fall back to — omitted (`None`) means exactly that: no overrides.

    `context_window_tokens` mirrors `model`'s idiom: omitted (`None`) falls
    back to `_DEFAULT_CONTEXT_WINDOW_TOKENS`, so this factory can never
    produce a profile with `context_window_tokens=None` -- only a
    directly-constructed `CoderProfile` can opt all the way out.

    `shell_allow_list` is the odd one out: unlike `model`/`context_window_tokens`,
    omitted (`None`) means exactly `None` here too -- the new default,
    unrestricted mode (decision 2026-07-07). It does NOT fall back to
    `_SHELL_ALLOW_LIST`; a caller that wants the restrictive reference list
    must pass it explicitly. `worker.py` resolves this from the kernel's
    `--shell-allow-list` flag (absent -> `all` -> `None`).
    """
    return CoderProfile(
        assistant_id="clipse-coder",
        model=model if model is not None else _DEFAULT_MODEL,
        system_prompt=_SYSTEM_PROMPT,
        shell_allow_list=tuple(shell_allow_list) if shell_allow_list is not None else None,
        model_params=model_params,
        context_window_tokens=(
            context_window_tokens if context_window_tokens is not None else _DEFAULT_CONTEXT_WINDOW_TOKENS
        ),
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
- If your context grows large, call the `compact_conversation` tool to \
summarize older history before continuing.
- When you have written the docs (or decided none are needed), stop. If a \
shell command is rejected, do not retry it in a loop — try another approach \
or report blocked.
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


def get_coder_docs_profile(
    model: str | None = None,
    model_params: dict[str, Any] | None = None,
    context_window_tokens: int | None = None,
    shell_allow_list: Sequence[str] | None = None,
) -> CoderProfile:
    """Return the DAC profile for the Coder lane's documentation sub-step.

    A distinct `assistant_id` ("clipse-coder-docs") keeps the docs turn's
    telemetry/checkpoints separable from the coding turn's; the model matches
    the coding turn by default (docs need no stronger model). Like
    `get_coder_profile`, `model` is a placeholder spec, never a live
    credential, and falls back to `_DEFAULT_MODEL` when omitted (`None`).

    `model_params` mirrors `get_coder_profile`'s: an opaque, no-default kwargs
    bag (config.ModelParams's `CoderDocs` map, threaded through as JSON via
    `worker.py`'s `--docs-model-params` flag) that stays `None` when omitted.

    `context_window_tokens` mirrors `get_coder_profile`'s idiom too: omitted
    (`None`) falls back to `_DEFAULT_CONTEXT_WINDOW_TOKENS`.

    `shell_allow_list` mirrors `get_coder_profile`'s own odd-one-out idiom:
    omitted (`None`) means exactly `None` -- unrestricted, the new default
    (decision 2026-07-07) -- never a fall back to `_DOCS_SHELL_ALLOW_LIST`.
    `worker.py` resolves this from the kernel's `--docs-shell-allow-list`
    flag (absent -> `all` -> `None`).
    """
    return CoderProfile(
        assistant_id="clipse-coder-docs",
        model=model if model is not None else _DEFAULT_MODEL,
        system_prompt=_DOCS_SYSTEM_PROMPT,
        shell_allow_list=tuple(shell_allow_list) if shell_allow_list is not None else None,
        model_params=model_params,
        context_window_tokens=(
            context_window_tokens if context_window_tokens is not None else _DEFAULT_CONTEXT_WINDOW_TOKENS
        ),
    )
