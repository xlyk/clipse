"""LangGraph StateGraph for the Coder lane.

Wraps `clipse_agent.dac` (the DAC engine wrapper) in a small graph that
gives the kernel a typed, testable seam around one DAC turn, per the
design doc's Coder graph:

    load_context -> ensure_worktree -> run_DAC -> {
        completed (clean)   -> run_docs -> commit -> push -> open_PR -> emit_result
        interrupted / over token ceiling -> emit_result
    }

`run_docs` is a best-effort documentation turn on the clean path only: it
writes docs into the same worktree so they ride the same commit/PR as the
code, and can never turn a review-ready run into `blocked` (see make_run_docs).

`run_DAC` is the only async node (it awaits `dac.drive_turn`), so the
compiled graph must always be driven with `.ainvoke`/`.astream`, never the
sync `.invoke` -- LangGraph raises `TypeError` from a sync `.invoke` the
moment it reaches an async-only node.

Outcome mapping (design doc "Board & state machine", `internal/board.go`'s
transition table): the Coder lane only ever runs while a card sits in the
`running` or `rework` column, and `internal/board.Next` treats `(done,
running)` as an *illegal* transition -- only `needs_review`, `blocked`, and
`continue` are legal there (see `TestNext_AllPairsCovered` /
`TestNext_ErrorNamesOutcomeAndCurrent` in `internal/board/board_test.go`).
So a clean DAC turn that opens/updates a PR always emits `needs_review`
(the PR is ready for the Reviewer lane), never `done` -- `done` belongs to
the Reviewer/Git-operator lanes' own terminal columns. This module
never emits `continue`: driving a single DAC turn to either "completed" or
"interrupted" (dac.py's only two outcome hints) fully determines the
result, with nothing left over that would need a same-thread respawn
without new input.

Safety: this module never calls `deepagents_code` directly -- it only
calls `dac.build_coder_agent` / `dac.drive_turn`, which already enforce the
non-negotiable `auto_approve=False, interrupt_shell_only=True` shell
wiring (see dac.py's module docstring and the design doc's "DAC API spike
findings"). Nothing here re-derives or could bypass that.
"""

from __future__ import annotations

import json
import os
import subprocess
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Any, TypedDict

from langgraph.graph import END, START, StateGraph

from clipse_agent import dac
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.profiles.coder import CoderProfile, get_coder_docs_profile, get_coder_profile

if TYPE_CHECKING:
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.graph.state import CompiledStateGraph

    from clipse_agent.dac import DacTurnResult


class CoderGraphError(RuntimeError):
    """Raised when a node hits an unrecoverable infrastructure error (a
    missing worktree, or a git/gh command that had to succeed but didn't).

    Nodes let this propagate out of `.ainvoke`/`.astream` rather than
    catching it -- guaranteeing *some* schema-valid result gets emitted
    even after an error like this is the worker entrypoint's job (it wraps
    the whole graph invocation), not this module's.
    """


# ---------------------------------------------------------------------------
# Subprocess seam (git/gh)
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class CommandResult:
    """The outcome of running one git/gh command."""

    returncode: int
    stdout: str
    stderr: str


# An injectable stand-in for `subprocess.run`: given an argv and a cwd, run
# it and report what happened. Tests pass a fake that records calls and
# replays canned results instead of touching a real git/gh binary.
CommandRunner = Callable[[Sequence[str], str], CommandResult]

# The real DAC-agent builder / turn driver this graph calls by default.
# Injectable so tests never build a real DAC agent or drive a real model.
AgentFactory = Callable[[CoderProfile, "BaseCheckpointSaver | None", str], tuple[Any, Any]]
TurnDriver = Callable[..., Awaitable["DacTurnResult"]]


def _default_run_command(argv: Sequence[str], cwd: str) -> CommandResult:
    proc = subprocess.run(list(argv), cwd=cwd, capture_output=True, text=True)
    return CommandResult(returncode=proc.returncode, stdout=proc.stdout, stderr=proc.stderr)


def _run(run_command: CommandRunner, argv: Sequence[str], cwd: str, *, check: bool = True) -> CommandResult:
    result = run_command(argv, cwd)
    if check and result.returncode != 0:
        raise CoderGraphError(
            f"command failed (exit {result.returncode}): {' '.join(argv)}\nstderr: {result.stderr}"
        )
    return result


# ---------------------------------------------------------------------------
# Graph state
# ---------------------------------------------------------------------------


class CoderState(TypedDict, total=False):
    """State threaded through the Coder graph for one worker turn.

    Every key is optional at the TypedDict level (`total=False`) because
    different nodes populate different subsets -- e.g. the blocked path
    never reaches `pr_url`/`artifacts`. `emit_result` uses `.get(...)`
    defaults for exactly that reason.
    """

    # --- Supplied by the caller (worker.py) at invocation ---
    issue_id: str
    run_id: str
    thread_id: str
    workspace: str
    branch: str  # if omitted, ensure_worktree derives it via `git rev-parse`
    base_branch: str  # base branch for a newly-created PR; default "main"
    issue_text: str  # falls back to $CLIPSE_ISSUE_TEXT if omitted
    review_feedback: str  # falls back to $CLIPSE_REVIEW_FEEDBACK if omitted
    turn_count: int  # turns already completed for this issue; default 0
    max_tokens: int | None
    resume_payload: Any | None  # non-None => resume a prior DAC interrupt
    prior_summary: str | None  # carried forward from a previous turn

    # --- load_context ---
    task_text: str

    # --- ensure_worktree ---
    cwd: str

    # --- run_DAC ---
    dac_outcome_hint: str  # "completed" | "interrupted" (dac.OutcomeHint)
    dac_summary: str
    tokens_in: int
    tokens_out: int
    interrupt_payload: list[Any] | None
    token_ceiling_exceeded: bool

    # --- run_docs (best-effort; kept OUT of the run_DAC channels above so a
    #     doc-turn ceiling/interrupt/failure can never route the run to blocked
    #     -- emit_result reads none of these for its blocked branches) ---
    doc_summary: str
    doc_tokens_in: int
    doc_tokens_out: int

    # --- commit ---
    artifacts: list[str]
    committed: bool

    # --- open_PR ---
    pr_url: str | None

    # --- emit_result ---
    result: WorkerResult


# Namespaces DAC's own checkpoint thread away from this wrapping graph's
# checkpoint thread. Both may be compiled against the *same* physical
# checkpointer (the design doc's "one checkpointer database per issue"),
# and AsyncSqliteSaver keys checkpoints by (thread_id, checkpoint_ns) --
# without this, two structurally different graphs writing checkpoints
# under the identical raw thread_id would corrupt each other's state.
# WorkerResult.thread_id still carries the raw, un-namespaced value: it
# names DAC's actual conversation, which is the thing that must resume
# correctly; the suffix is purely this module's internal bookkeeping.
_DAC_THREAD_NAMESPACE_SUFFIX = "::dac"


def _dac_config(thread_id: str) -> dict[str, Any]:
    return {"configurable": {"thread_id": f"{thread_id}{_DAC_THREAD_NAMESPACE_SUFFIX}"}}


# The documentation turn (run_docs) drives a SECOND DAC agent -- a different
# system prompt + allow-list -- against the same per-issue checkpointer. It
# needs its own thread namespace so it never resumes the coding turn's message
# history (mirrors reviewer.py's `::review-dac`).
_DOCS_DAC_THREAD_NAMESPACE_SUFFIX = "::docs-dac"


def _docs_dac_config(thread_id: str) -> dict[str, Any]:
    return {"configurable": {"thread_id": f"{thread_id}{_DOCS_DAC_THREAD_NAMESPACE_SUFFIX}"}}


# ---------------------------------------------------------------------------
# load_context
# ---------------------------------------------------------------------------


def load_context(state: CoderState) -> dict[str, Any]:
    """Compose this turn's DAC prompt from the issue text (args/env), any
    summary a previous turn left behind, and any review feedback that routed
    the card back to this lane for a rework re-run.

    `issue_text` normally arrives via the invocation input (set by
    worker.py from its own args); the `$CLIPSE_ISSUE_TEXT` env fallback
    covers callers that pass it that way instead. `prior_summary` is not
    always explicitly supplied by the caller -- when this graph is
    compiled with a checkpointer and invoked again with the same
    thread_id, it also arrives automatically as whatever `emit_result` set
    on the previous turn (see `build_coder_graph`'s docstring).

    `review_feedback` mirrors `issue_text`'s input/env handling
    (`$CLIPSE_REVIEW_FEEDBACK`, injected by the dispatcher only for a Coder
    re-run claimed out of the rework column). Unlike `prior_summary` (this
    lane's OWN previous-turn summary), it is a DIFFERENT lane's verdict -- the
    reviewer's changes_requested, or a git-operator stale-base conflict -- so
    it is folded in under its own clearly-delimited heading and, being the
    most recent and most actionable instruction, last. A fresh run has none
    and the prompt is unchanged from before this feature.
    """
    issue_text = state.get("issue_text") or os.environ.get("CLIPSE_ISSUE_TEXT", "")
    prior_summary = state.get("prior_summary")
    review_feedback = state.get("review_feedback") or os.environ.get("CLIPSE_REVIEW_FEEDBACK", "")

    task_text = issue_text
    if prior_summary:
        task_text = f"{task_text}\n\nProgress from a previous turn on this issue:\n{prior_summary}"
    if review_feedback:
        task_text = f"{task_text}\n\nThe previous review requested these changes; address them:\n{review_feedback}"

    return {"task_text": task_text}


# ---------------------------------------------------------------------------
# ensure_worktree
# ---------------------------------------------------------------------------


def make_ensure_worktree(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Validate the worktree the kernel already created for this issue.

    Does not `os.chdir` the process -- every downstream git/gh call gets
    `cwd` passed explicitly instead, so this stays side-effect-free at the
    process level (important since a test suite runs many graphs in one
    process).
    """

    def _node(state: CoderState) -> dict[str, Any]:
        workspace = state.get("workspace")
        if not workspace:
            raise CoderGraphError("ensure_worktree: no workspace path given")

        path = Path(workspace)
        if not path.is_dir():
            raise CoderGraphError(f"ensure_worktree: worktree does not exist: {workspace}")
        if not (path / ".git").exists():
            raise CoderGraphError(f"ensure_worktree: not a git worktree (missing .git): {workspace}")

        cwd = str(path.resolve())
        updates: dict[str, Any] = {"cwd": cwd}

        if not state.get("branch"):
            head = _run(run_command, ["git", "rev-parse", "--abbrev-ref", "HEAD"], cwd)
            updates["branch"] = head.stdout.strip()

        return updates

    return _node


# ---------------------------------------------------------------------------
# run_DAC
# ---------------------------------------------------------------------------


def make_run_dac(
    profile: CoderProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
) -> Callable[[CoderState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh `task_text` turn normally, or a
    `resume` of a previously-interrupted turn when `resume_payload` is set.
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
        config = _dac_config(state["thread_id"])
        max_tokens = state.get("max_tokens")
        resume_payload = state.get("resume_payload")

        if resume_payload is not None:
            turn_result = await turn_driver(agent_graph, config, resume=resume_payload, max_tokens=max_tokens)
        else:
            turn_result = await turn_driver(
                agent_graph, config, task_text=state.get("task_text", ""), max_tokens=max_tokens
            )

        return {
            "dac_outcome_hint": turn_result.outcome_hint,
            "dac_summary": turn_result.final_text,
            "tokens_in": turn_result.tokens_in,
            "tokens_out": turn_result.tokens_out,
            "interrupt_payload": turn_result.interrupt_payload,
            "token_ceiling_exceeded": turn_result.token_ceiling_exceeded,
        }

    return _node


def route_after_dac(state: CoderState) -> str:
    """Pick the next node once `run_DAC` has run.

    A token-ceiling abort or a genuine interrupt both skip straight to
    `emit_result` as blocked (see the module docstring on why `blocked`
    covers both); anything else proceeds to the documentation turn (which
    then flows to commit). Docs therefore never run when the code turn didn't
    produce a review-ready change.
    """
    if state.get("token_ceiling_exceeded") or state.get("interrupt_payload") is not None:
        return "emit_result"
    return "run_docs"


# ---------------------------------------------------------------------------
# run_docs (documentation sub-step; best-effort, clean path only)
# ---------------------------------------------------------------------------


def _docs_task_text(state: CoderState) -> str:
    """Compose the documentation turn's prompt.

    Unlike load_context's coding prompt, this points the docs agent at the
    change the coding turn just made in THIS worktree (still uncommitted) and
    asks it to update docs if warranted -- a no-op is a valid outcome.
    """
    issue_id = state.get("issue_id", "")
    issue_text = state.get("issue_text") or os.environ.get("CLIPSE_ISSUE_TEXT", "")
    code_summary = (state.get("dac_summary") or "").strip()

    parts = [
        f"The Coder lane just edited this worktree to implement {issue_id}; "
        "the change is not committed yet.",
    ]
    if issue_text:
        parts.append(f"Issue:\n{issue_text}")
    if code_summary:
        parts.append(f"What the Coder reported doing:\n{code_summary}")
    parts.append(
        "Inspect the uncommitted change with `git status` and `git diff`, then update "
        "or add documentation if the change is user- or contributor-facing and the docs "
        "don't already cover it. If nothing needs documenting, make no file changes at "
        "all -- a no-op is expected. Do not edit source code."
    )
    return "\n\n".join(parts)


def _docs_max_tokens(state: CoderState) -> int | None:
    """The docs turn's token budget: the SAME per-round ceiling the coding
    turn used, not `ceiling - cumulative_spent`.

    `drive_turn`'s ceiling is per-round, not cumulative (it caps the largest
    single round's input tokens -- see its docstring); the docs turn is a
    SEPARATE DAC turn with its own per-round guard, so it must receive that
    same ceiling unchanged. Deducting the coding turn's cumulative spend here
    was a leftover from the old cumulative-pool model: after a big coding
    turn, `ceiling - spent` could land at (or near) zero and trip the docs
    turn's ceiling on its very first round -- best-effort-skipping docs
    whenever the coding turn ran long, regardless of how large `max_tokens`
    actually is. `None` (no ceiling configured) still stays `None`.
    """
    return state.get("max_tokens")


def make_run_docs(
    profile: CoderProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
) -> Callable[[CoderState], Awaitable[dict[str, Any]]]:
    """Drive one best-effort documentation DAC turn in the coder's worktree.

    Runs only on the clean path (see route_after_dac), after the coding turn
    already produced a review-ready change. Any docs the agent writes sit next
    to the code edits and are folded into the same commit by `make_commit`'s
    `git add -A` -- same commit, same PR, no separate branch.

    Best-effort by construction: it returns ONLY the private `doc_*` keys and
    never `interrupt_payload`/`token_ceiling_exceeded`/`dac_summary`, so a
    doc-turn ceiling, interrupt, or outright exception can neither turn this
    run into `blocked` nor rename the PR -- the coding turn's review-ready PR
    ships regardless. A ceiling/interrupt on the *coding* turn, by contrast,
    still blocks (route_after_dac skips this node entirely), because there the
    change itself may be incomplete or need a human. Uses its own `::docs-dac`
    thread namespace so it never resumes the coding turn's message history.
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        try:
            agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
            turn_result = await turn_driver(
                agent_graph,
                _docs_dac_config(state["thread_id"]),
                task_text=_docs_task_text(state),
                max_tokens=_docs_max_tokens(state),
            )
        except Exception as exc:  # noqa: BLE001 -- docs are non-critical; degrade, never block the PR
            return {"doc_summary": f"Documentation step skipped (error): {exc}", "doc_tokens_in": 0, "doc_tokens_out": 0}

        if turn_result.token_ceiling_exceeded:
            summary = "Documentation step skipped: token budget reached before it finished."
        elif turn_result.interrupt_payload is not None:
            summary = "Documentation step skipped: it needed input it could not resolve on its own."
        else:
            summary = turn_result.final_text

        return {
            "doc_summary": summary,
            "doc_tokens_in": turn_result.tokens_in,
            "doc_tokens_out": turn_result.tokens_out,
        }

    return _node


# ---------------------------------------------------------------------------
# commit
# ---------------------------------------------------------------------------


def _parse_porcelain_paths(porcelain_output: str) -> list[str]:
    """Extract file paths from `git status --porcelain` (v1 format).

    Each line is `XY path` (a 2-character status code, a space, then the
    path) or `XY orig -> new` for a rename, in which case the path is the
    new name.
    """
    paths: list[str] = []
    for line in porcelain_output.splitlines():
        if not line:
            continue
        path = line[3:] if len(line) > 3 else line.strip()
        if " -> " in path:
            path = path.split(" -> ", 1)[1]
        paths.append(path)
    return paths


def _commit_message(state: CoderState) -> str:
    issue_id = state.get("issue_id", "")
    turn = state.get("turn_count", 0) + 1
    summary_lines = (state.get("dac_summary") or "").strip().splitlines()
    headline = summary_lines[0] if summary_lines else f"turn {turn}"
    return f"{issue_id}: {headline}"[:72]


def make_commit(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Stage and commit whatever this turn changed.

    `git add -A` always runs (harmless when there's nothing to add); the
    actual `git commit` is skipped when `git status --porcelain` reports
    no changes at all, so a turn that only left commentary (no file
    edits) doesn't fail on "nothing to commit".
    """

    def _node(state: CoderState) -> dict[str, Any]:
        cwd = state["cwd"]
        _run(run_command, ["git", "add", "-A"], cwd)
        status = _run(run_command, ["git", "status", "--porcelain"], cwd)
        paths = _parse_porcelain_paths(status.stdout)

        committed = False
        if paths:
            _run(run_command, ["git", "commit", "-m", _commit_message(state)], cwd)
            committed = True

        return {"artifacts": paths, "committed": committed}

    return _node


# ---------------------------------------------------------------------------
# push
# ---------------------------------------------------------------------------


def make_push(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Push the branch. `--set-upstream` makes this safe to call every
    turn, whether or not this is the branch's first push."""

    def _node(state: CoderState) -> dict[str, Any]:
        _run(run_command, ["git", "push", "--set-upstream", "origin", state["branch"]], state["cwd"])
        return {}

    return _node


# ---------------------------------------------------------------------------
# open_PR
# ---------------------------------------------------------------------------


def _pr_title(state: CoderState) -> str:
    issue_id = state.get("issue_id", "")
    lines = (state.get("dac_summary") or "").strip().splitlines()
    headline = lines[0] if lines else "Implement issue"
    return f"{issue_id}: {headline}"[:120]


def _pr_body(state: CoderState) -> str:
    issue_id = state.get("issue_id", "")
    summary = (state.get("dac_summary") or "").strip()
    body = f"Implements {issue_id}."
    if summary:
        body += f"\n\n{summary}"
    doc_summary = (state.get("doc_summary") or "").strip()
    if doc_summary:
        body += f"\n\nDocumentation: {doc_summary}"
    return body


def make_open_pr(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Idempotently open (or reuse) the PR for this issue's branch.

    `gh pr view <branch> --json url` first; a PR already exists iff that
    exits zero, in which case its URL is reused verbatim. Only when it
    reports nothing does this fall through to `gh pr create` -- so a crash
    after a previous turn's push-but-before-record, or an auto-continuation
    turn, appends commits to the existing branch/PR instead of opening a
    second one.

    Created PRs always pass `--draft`: a Coder-lane turn ending cleanly means
    DAC finished its turn, not that the change has been reviewed -- the
    Reviewer lane (Phase 3) is what should take a PR out of draft once it
    actually is ready, so a coder-authored PR never implies "ready to merge"
    on its own.
    """

    def _node(state: CoderState) -> dict[str, Any]:
        branch = state["branch"]
        cwd = state["cwd"]

        view = _run(run_command, ["gh", "pr", "view", branch, "--json", "url"], cwd, check=False)
        if view.returncode == 0:
            return {"pr_url": json.loads(view.stdout)["url"]}

        base_branch = state.get("base_branch") or "main"
        created = _run(
            run_command,
            [
                "gh",
                "pr",
                "create",
                "--draft",
                "--head",
                branch,
                "--base",
                base_branch,
                "--title",
                _pr_title(state),
                "--body",
                _pr_body(state),
            ],
            cwd,
        )
        # `gh pr create` prints the created PR's URL as its last line of
        # stdout on success (it has no --json flag, verified against the
        # installed gh CLI's own --help).
        url = next((line.strip() for line in reversed(created.stdout.splitlines()) if line.strip()), "")
        return {"pr_url": url}

    return _node


# ---------------------------------------------------------------------------
# emit_result
# ---------------------------------------------------------------------------


def _capability_summary(tokens_in: int, tokens_out: int) -> str:
    total = tokens_in + tokens_out
    return f"Aborted: exceeded this run's token budget after spending {total} tokens ({tokens_in} in / {tokens_out} out)."


def _needs_input_summary(interrupt_payload: list[Any]) -> str:
    return f"DAC paused for input it can't resolve on its own: {interrupt_payload!r}"


def _needs_review_summary(state: CoderState) -> str:
    parts: list[str] = []
    dac_summary = (state.get("dac_summary") or "").strip()
    if dac_summary:
        parts.append(dac_summary)
    parts.append("Committed and pushed changes." if state.get("committed") else "No new changes to commit this turn.")
    doc_summary = (state.get("doc_summary") or "").strip()
    if doc_summary:
        parts.append(f"Docs: {doc_summary}")
    pr_url = state.get("pr_url")
    if pr_url:
        parts.append(f"PR: {pr_url}")
    return " ".join(parts)


def emit_result(state: CoderState) -> dict[str, Any]:
    """Map this turn's outcome onto the shared `contract.WorkerResult`.

    Only ever produces `needs_review` or `blocked` -- never `done`:
    `internal/board.Next` treats `(done, running)`/`(done, rework)` as
    illegal transitions (the Coder lane's card lives in `running`/`rework`
    while this graph runs), so `done` is reserved for the
    Reviewer/Git-operator lanes' own terminal columns. A
    token-ceiling abort takes priority over an interrupt (dac.py's own
    documented precedence), and both map to `blocked` with a distinct
    `block_kind` -- present here in every branch, consistent with
    amendment X2's "present iff outcome == blocked" invariant.

    Also returns `prior_summary`: whatever DAC said this turn, threaded
    forward so a later turn on the same checkpointed thread sees it via
    `load_context` without the caller having to resupply it explicitly.
    """
    # Sum the coding turn and the (best-effort) docs turn. The docs turn wrote
    # separate doc_* keys precisely so it couldn't clobber the coding turn's
    # counts here (CoderState channels are last-write-wins). On the blocked
    # paths docs never ran, so the doc_* keys are absent and .get(..., 0)
    # leaves the coding-turn totals untouched.
    tokens_in = state.get("tokens_in", 0) + state.get("doc_tokens_in", 0)
    tokens_out = state.get("tokens_out", 0) + state.get("doc_tokens_out", 0)
    tokens = Tokens(**{"in": tokens_in, "out": tokens_out})
    turn_count = state.get("turn_count", 0) + 1
    common: dict[str, Any] = {
        "run_id": state["run_id"],
        "issue_id": state["issue_id"],
        "lane": Lane.coder,
        "thread_id": state["thread_id"],
        "turn_count": turn_count,
        "tokens": tokens,
    }

    interrupt_payload = state.get("interrupt_payload")
    if state.get("token_ceiling_exceeded"):
        result = WorkerResult(
            **common,
            outcome=Outcome.blocked,
            block_kind=BlockKind.capability,
            summary=_capability_summary(tokens_in, tokens_out),
            artifacts=state.get("artifacts", []),
        )
    elif interrupt_payload is not None:
        result = WorkerResult(
            **common,
            outcome=Outcome.blocked,
            block_kind=BlockKind.needs_input,
            summary=_needs_input_summary(interrupt_payload),
            artifacts=state.get("artifacts", []),
        )
    else:
        result = WorkerResult(
            **common,
            outcome=Outcome.needs_review,
            summary=_needs_review_summary(state),
            artifacts=state.get("artifacts", []),
            pr_url=state.get("pr_url"),
        )

    # Thread turn_count forward on the checkpointed state (like prior_summary)
    # so a continuation turn on the same thread_id increments from the real
    # prior value instead of resetting to 1 each invocation.
    return {
        "result": result,
        "prior_summary": state.get("dac_summary", ""),
        "turn_count": turn_count,
    }


# ---------------------------------------------------------------------------
# Graph assembly
# ---------------------------------------------------------------------------


def build_coder_graph(
    *,
    checkpointer: BaseCheckpointSaver | None = None,
    profile: CoderProfile | None = None,
    docs_profile: CoderProfile | None = None,
    agent_factory: AgentFactory = dac.build_coder_agent,
    turn_driver: TurnDriver = dac.drive_turn,
    run_command: CommandRunner | None = None,
) -> CompiledStateGraph[Any, Any, Any, Any]:
    """Build and compile the Coder lane's graph.

    `agent_factory`/`turn_driver` default to the real `clipse_agent.dac`
    module (`build_coder_agent`/`drive_turn`); `run_command` defaults to a
    real `subprocess.run`-backed runner. Tests override all three so no
    real model, DAC agent, subprocess, or network call is ever touched.

    Compiled with `checkpointer` (LangGraph resume support), which the
    same call also forwards into `agent_factory` so DAC's own agent shares
    one physical checkpoint store with this wrapping graph (design doc:
    "one checkpointer database per issue") -- see `_dac_config` for how
    the two avoid colliding on thread identity within that shared store.
    One concrete effect of compiling with a checkpointer: invoking the
    returned graph twice with the same `configurable.thread_id` lets
    `load_context` see the *previous* call's `prior_summary` automatically
    (LangGraph loads the thread's checkpointed state before merging in the
    new input), with no need for the caller to resupply it.

    `run_DAC` and `run_docs` are the async nodes (both await a DAC turn), so
    the returned graph must be driven with `.ainvoke`/`.astream` -- never the
    sync `.invoke`. Both turns share `agent_factory`/`turn_driver`; only the
    profile differs (`profile` for the coding turn, `docs_profile` for the
    documentation turn).
    """
    resolved_profile = profile if profile is not None else get_coder_profile()
    resolved_docs_profile = docs_profile if docs_profile is not None else get_coder_docs_profile()
    resolved_run_command = run_command if run_command is not None else _default_run_command

    graph: StateGraph[CoderState, Any, Any, Any] = StateGraph(CoderState)
    graph.add_node("load_context", load_context)
    graph.add_node("ensure_worktree", make_ensure_worktree(resolved_run_command))
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer))
    graph.add_node("run_docs", make_run_docs(resolved_docs_profile, agent_factory, turn_driver, checkpointer))
    graph.add_node("commit", make_commit(resolved_run_command))
    graph.add_node("push", make_push(resolved_run_command))
    graph.add_node("open_PR", make_open_pr(resolved_run_command))
    graph.add_node("emit_result", emit_result)

    graph.add_edge(START, "load_context")
    graph.add_edge("load_context", "ensure_worktree")
    graph.add_edge("ensure_worktree", "run_DAC")
    graph.add_conditional_edges("run_DAC", route_after_dac)
    graph.add_edge("run_docs", "commit")
    graph.add_edge("commit", "push")
    graph.add_edge("push", "open_PR")
    graph.add_edge("open_PR", "emit_result")
    graph.add_edge("emit_result", END)

    return graph.compile(checkpointer=checkpointer)
