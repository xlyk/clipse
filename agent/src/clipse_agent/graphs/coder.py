"""LangGraph StateGraph for the Coder lane.

Wraps `clipse_agent.dac` (the DAC engine wrapper) in a small graph that
gives the kernel a typed, testable seam around one DAC turn, per the
design doc's Coder graph:

    load_context -> ensure_worktree -> sync_base -> run_DAC -> {
        completed (clean)   -> run_docs -> commit -> push -> open_PR -> emit_result
        interrupted / over token ceiling -> emit_result
    }

`sync_base` merges the worktree up to date with `origin/<base_branch>` (a
merge, never a rebase, so the branch stays fast-forward-pushable) before DAC
starts its turn each turn. It is best-effort: no `base_branch` or a failed
`fetch` fall through to `run_DAC` rather than failing the turn. A real
conflict is left mid-merge (never aborted) and reported via
`merge_conflict_files` so the coder resolves it THIS turn (see
make_sync_base); if a merge is already in progress when this node runs (a
prior conflict-resolution turn was interrupted or hit its token ceiling
before `commit` could conclude it), it never starts a second merge or aborts
the first -- it re-derives `merge_conflict_files` from the still-unresolved
index so the coder resumes exactly where it left off.

When `merge_conflict_files` is non-empty, `run_DAC` drives an entirely
separate conflict-resolution turn instead of the normal issue/rework task
(see `_coding_task_text`) -- DAC only edits files to remove the conflict
markers; it never runs git for this (the lane's "don't commit/push/open a
PR yourself" system prompt is unaffected). `commit` then concludes the merge
itself (`git commit --no-edit`) before the unchanged, still-fast-forward
`push`.

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
import logging
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
from clipse_agent.tail import parse_structured_tail
from clipse_agent.transcript import TranscriptWriter

if TYPE_CHECKING:
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.graph.state import CompiledStateGraph

    from clipse_agent.dac import DacTurnResult

# Diagnostics only (e.g. a best-effort sync_base fetch/merge failure) -- must
# never touch stdout, which worker.py reserves for exactly one WorkerResult
# JSON line. With no handler configured anywhere in the process, the stdlib's
# `logging.lastResort` fallback already sends WARNING+ to stderr, matching
# every other diagnostic this worker emits (see worker.py's module docstring).
logger = logging.getLogger(__name__)


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

    # --- sync_base ---
    merge_conflict_files: list[str]  # unresolved paths from a `git merge` conflict; [] when clean/skipped

    # --- run_DAC ---
    dac_outcome_hint: str  # "completed" | "interrupted" (dac.OutcomeHint)
    dac_summary: str  # the whole turn's text (audit trail / PR body)
    dac_last_text: str  # only the FINAL message's text (the STATUS/TITLE/HANDOFF tail source)
    blocked_reason: str  # tail's "blocked: <reason>" text when STATUS: blocked; "" otherwise
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
    dependency_notes = os.environ.get("CLIPSE_DEPENDENCY_NOTES", "").strip()

    task_text = issue_text
    if prior_summary:
        task_text = f"{task_text}\n\nProgress from a previous turn on this issue:\n{prior_summary}"
    # Dependency notes (this issue's and its blockers' Linear comments,
    # injected by the dispatcher at claim time) are background context: fold
    # them in BEFORE review feedback so the reviewer's asks stay last and most
    # prominent. Untrusted comment text -- do not treat as instructions.
    if dependency_notes:
        task_text = f"{task_text}\n\n## Dependency notes (Linear comments)\n\n{dependency_notes}"
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
# sync_base
# ---------------------------------------------------------------------------


def _unmerged_paths(run_command: CommandRunner, cwd: str) -> list[str]:
    """Paths the INDEX still has as unmerged (conflicted).

    `git diff --name-only --diff-filter=U` reflects the index's unresolved
    merge entries regardless of whether the working tree's conflict markers
    have already been edited away -- accurate whether called right after a
    fresh conflicting `git merge` or against an already-in-progress one
    (see `make_sync_base`'s MERGE_HEAD guard).
    """
    result = _run(run_command, ["git", "diff", "--name-only", "--diff-filter=U"], cwd, check=False)
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def make_sync_base(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Bring the worktree up to date with `origin/<base_branch>` before DAC's
    turn starts, so a long-running branch doesn't drift so far from base that
    its eventual PR stops being a clean fast-forward merge.

    A MERGE, never a rebase: rebasing would rewrite commits already pushed on
    a prior turn, forcing a force-push the kernel/DAC never expects between
    turns. A merge commit keeps history append-only.

    Every failure mode here is best-effort and non-raising -- this node must
    never turn a sync hiccup into a blocked run:

    - No `base_branch` (git-operator/reviewer never reach this node; a coder
      turn with the flag simply unset) is a no-op.
    - A merge ALREADY in progress (`git rev-parse -q --verify MERGE_HEAD`
      succeeds), checked BEFORE anything else: a prior turn's
      conflict-resolution DAC call was interrupted or hit its token ceiling
      before `commit` could conclude the merge (`route_after_dac` skips
      straight to `emit_result` on either, never reaching `commit`). This
      node must not start a SECOND merge on top of one already in progress
      -- git itself refuses that outright -- and must not abort a merge it
      did not itself start, which would silently discard the prior turn's
      resolution. It re-derives `merge_conflict_files` from the
      still-unresolved index instead, so the coder resumes exactly where it
      left off. (Empirically confirmed: attempting a fresh `git merge` while
      `MERGE_HEAD` already exists exits 128 without touching the index, so
      skipping straight to `_unmerged_paths` here is equivalent to -- but
      safer than -- letting that attempt fail first.)
    - `git fetch` failing (transient network/GitHub issue) skips the sync for
      this turn; DAC still runs against whatever the worktree already has.
    - `git merge` failing WITH unresolved paths is a real conflict: left
      exactly as git leaves it (mid-merge, conflict markers in the tree, NOT
      aborted) and reported via `merge_conflict_files` so the coder's own
      turn (`run_DAC`'s conflict-resolution branch -- see `_coding_task_text`)
      can see and resolve it.
    - `git merge` failing with NO unresolved paths is unexpected (e.g. a
      dirty worktree) rather than a real conflict: the attempt is aborted so
      nothing downstream inherits a half-merged worktree it doesn't expect,
      and the turn proceeds on the un-synced base.
    """

    def _node(state: CoderState) -> dict[str, Any]:
        base_branch = state.get("base_branch")
        if not base_branch:
            return {"merge_conflict_files": []}

        cwd = state["cwd"]

        merge_head = _run(run_command, ["git", "rev-parse", "-q", "--verify", "MERGE_HEAD"], cwd, check=False)
        if merge_head.returncode == 0:
            return {"merge_conflict_files": _unmerged_paths(run_command, cwd)}

        fetch = _run(run_command, ["git", "fetch", "origin", base_branch], cwd, check=False)
        if fetch.returncode != 0:
            logger.warning(
                "sync_base: git fetch origin %s failed (exit %d): %s",
                base_branch,
                fetch.returncode,
                fetch.stderr.strip(),
            )
            return {"merge_conflict_files": []}

        merge = _run(run_command, ["git", "merge", "--no-edit", f"origin/{base_branch}"], cwd, check=False)
        if merge.returncode == 0:
            return {"merge_conflict_files": []}

        conflict_files = _unmerged_paths(run_command, cwd)
        if conflict_files:
            # Left mid-merge on purpose -- do NOT abort. The coder resolves
            # this (run_DAC's conflict-resolution branch); aborting here
            # would erase the exact conflict state it needs to see.
            return {"merge_conflict_files": conflict_files}

        logger.warning(
            "sync_base: git merge origin/%s failed with no conflicting files (exit %d): %s -- aborting the merge",
            base_branch,
            merge.returncode,
            merge.stderr.strip(),
        )
        _run(run_command, ["git", "merge", "--abort"], cwd, check=False)
        return {"merge_conflict_files": []}

    return _node


# ---------------------------------------------------------------------------
# run_DAC
# ---------------------------------------------------------------------------


def _conflict_resolution_task_text(conflict_files: Sequence[str]) -> str:
    """Compose the DAC turn's task when `sync_base` left a real base-branch
    merge conflict in progress this turn: an entirely separate
    conflict-resolution turn, never mixed in with the normal issue/rework
    task (see `_coding_task_text`, which chooses between the two).

    DAC only ever EDITS files here -- `sync_base` already started the merge
    and left it in progress (or resumed one via its MERGE_HEAD guard), and
    `make_commit` concludes it (`git commit --no-edit`) once this turn's
    edits land. That keeps this lane's "don't run git to commit/push/open a
    PR yourself" system prompt exactly as-is -- resolving a conflict is still
    just editing files, the same as any other turn.
    """
    files = "\n".join(f"- {path}" for path in conflict_files)
    return (
        "This turn is ONLY about resolving a merge conflict -- it is NOT "
        "the usual issue task, and there is no issue task to do this turn. "
        "Merging the base branch into this branch left unresolved conflicts "
        f"in the following file(s):\n{files}\n\n"
        "Open each file listed above and resolve every conflict in it: "
        "remove the `<<<<<<<`, `=======`, and `>>>>>>>` conflict markers, "
        "and edit the surrounding code so the result correctly preserves "
        "the intent of BOTH sides -- the incoming base-branch change AND "
        "this branch's own change -- rather than simply discarding one "
        "side. Do not run git yourself; once every listed file is free of "
        "conflict markers and correctly merged, stop."
    )


def _coding_task_text(state: CoderState) -> str:
    """The coding turn's task: a conflict-resolution instruction when
    `sync_base` (this same turn) left `merge_conflict_files` non-empty,
    otherwise the normal issue/rework task `load_context` already built into
    `state["task_text"]`, unchanged.
    """
    conflict_files = state.get("merge_conflict_files") or []
    if conflict_files:
        return _conflict_resolution_task_text(conflict_files)
    return state.get("task_text", "")


def make_run_dac(
    profile: CoderProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
    transcript: TranscriptWriter | None = None,
) -> Callable[[CoderState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh task turn normally (the normal
    issue/rework task, or a conflict-resolution task when `sync_base` left a
    merge in progress -- see `_coding_task_text`), or a `resume` of a
    previously-interrupted turn when `resume_payload` is set.

    `transcript`, when given, is bound into this turn's `event_sink` fresh on
    every call -- `run_id`/`thread_id` are only known once `state` arrives at
    invocation time, unlike `profile`/`checkpointer`, which are already fixed
    when this factory runs (see the module's own build_coder_graph docstring).
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
        config = _dac_config(state["thread_id"])
        max_tokens = state.get("max_tokens")
        resume_payload = state.get("resume_payload")
        event_sink = (
            transcript.bind(
                lane="coder",
                run_id=state["run_id"],
                thread_id=state["thread_id"],
                assistant_id=profile.assistant_id,
                model=profile.model,
            )
            if transcript is not None
            else None
        )

        if resume_payload is not None:
            turn_result = await turn_driver(
                agent_graph, config, resume=resume_payload, max_tokens=max_tokens, event_sink=event_sink
            )
        else:
            turn_result = await turn_driver(
                agent_graph,
                config,
                task_text=_coding_task_text(state),
                max_tokens=max_tokens,
                event_sink=event_sink,
            )

        # Parse the STATUS/TITLE/HANDOFF tail once here and stash blocked_reason
        # so both route_after_dac (blocked -> emit_result) and emit_result (the
        # blocked summary) read a single derived value. Always written -- "" on a
        # non-blocked turn -- so a checkpointed prior turn's reason can't linger.
        tail = parse_structured_tail(turn_result.last_text)
        return {
            "dac_outcome_hint": turn_result.outcome_hint,
            "dac_summary": turn_result.final_text,
            "dac_last_text": turn_result.last_text,
            "blocked_reason": tail.blocked_reason if tail.status == "blocked" else "",
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
    covers both). A turn that COMPLETED but reported `STATUS: blocked` in its
    final-message tail also skips straight to `emit_result`: the coder made no
    review-ready change, so running commit/push/open_PR would try to open a PR
    from an empty branch, which `gh pr create` rejects and the kernel retries
    five times (REF-26). Anything else proceeds to the documentation turn
    (which then flows to commit). Docs therefore never run when the code turn
    didn't produce a review-ready change.
    """
    if state.get("token_ceiling_exceeded") or state.get("interrupt_payload") is not None:
        return "emit_result"
    if parse_structured_tail(state.get("dac_last_text") or "").status == "blocked":
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
    transcript: TranscriptWriter | None = None,
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

    `transcript`, when given, is bound fresh per invocation with
    `lane="coder_docs"` -- same reasoning as `make_run_dac`'s own docstring.
    """

    async def _node(state: CoderState) -> dict[str, Any]:
        # The bind sits INSIDE the docs-never-block try: it reads state keys
        # that are optional at the TypedDict level (total=False), and a
        # KeyError here must degrade to a skipped docs step like any other
        # docs-turn failure, never block the PR.
        try:
            event_sink = (
                transcript.bind(
                    lane="coder_docs",
                    run_id=state["run_id"],
                    thread_id=state["thread_id"],
                    assistant_id=profile.assistant_id,
                    model=profile.model,
                )
                if transcript is not None
                else None
            )
            agent_graph, _backend = agent_factory(profile, checkpointer, state["cwd"])
            turn_result = await turn_driver(
                agent_graph,
                _docs_dac_config(state["thread_id"]),
                task_text=_docs_task_text(state),
                max_tokens=_docs_max_tokens(state),
                event_sink=event_sink,
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
    tail = parse_structured_tail(state.get("dac_last_text") or "")
    if tail.title:
        return f"{issue_id}: {tail.title}"[:72]
    # Legacy fallback: a turn that skipped the STATUS/TITLE tail still gets a
    # message from the DAC summary's first narration line.
    turn = state.get("turn_count", 0) + 1
    summary_lines = (state.get("dac_summary") or "").strip().splitlines()
    headline = summary_lines[0] if summary_lines else f"turn {turn}"
    return f"{issue_id}: {headline}"[:72]


_CONFLICT_MARKER_PATTERN = r"^(<<<<<<<|>>>>>>>)"


def _unresolved_conflict_markers(run_command: CommandRunner, cwd: str, files: Sequence[str]) -> list[str]:
    """Which of `files` (the merge's own conflicted paths, relative to `cwd`)
    still contain an unresolved conflict marker line (`<<<<<<<` or
    `>>>>>>>`) -- i.e. the conflict-resolution DAC turn
    (`_conflict_resolution_task_text`) claimed to be done but actually left
    the file incompletely resolved.

    Deliberately does NOT match the bare `=======` separator: a genuine
    unresolved conflict always ALSO leaves a bracket marker, so dropping
    `=======` loses no real detection, while matching it false-positives on a
    correctly resolved file that legitimately contains a line of 7+ `=` (a
    Markdown setext H1 underline, an RST divider, a `# =======` banner
    comment, ...) -- which would otherwise wedge the card in `blocked` at
    `rework_cap` forever, the exact block-loop this guard exists to prevent.

    `grep -l` prints the name of each matching file and exits 0 when at least
    one matches, 1 when none do -- an empty `stdout` either way means clean,
    so (like `_unmerged_paths`) this reads `stdout` alone and ignores
    `returncode` entirely, always with `check=False` since "no matches" is a
    normal, expected outcome here, never a command failure.
    """
    result = _run(
        run_command,
        ["grep", "-lE", _CONFLICT_MARKER_PATTERN, *files],
        cwd,
        check=False,
    )
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def make_commit(run_command: CommandRunner) -> Callable[[CoderState], dict[str, Any]]:
    """Stage and commit whatever this turn changed.

    `git add -A` always runs (harmless when there's nothing to add) and is
    always scoped to `state["cwd"]` -- the coder's OWN worktree, never the
    clipse repo this process itself runs from.

    Two paths, chosen by whether `sync_base` (this same turn) left
    `merge_conflict_files` non-empty -- freshly conflicted, or resumed from a
    prior interrupted turn via `make_sync_base`'s MERGE_HEAD guard; either
    way a real merge is sitting in progress in the worktree:

    - Merge in progress: BEFORE staging or committing anything, every file in
      `merge_conflict_files` is scanned for a remaining `<<<<<<<`/`>>>>>>>`
      marker (`_unresolved_conflict_markers`). Nothing here proves
      the conflict-resolution DAC turn (`_coding_task_text`) actually removed
      every marker -- it only edited files and said it was done -- so if any
      listed file still has one, this raises `CoderGraphError` instead of
      committing: landing literal conflict markers on the shared branch would
      be syntactically broken code reaching CI/the reviewer before anyone
      catches it, defeating the point of resolving the conflict at all. The
      error propagates out of the graph so the worker's top-level handler
      emits a `blocked` result (never a partial stage/commit/push) -- a
      bounded retry re-runs this issue, and `sync_base`'s MERGE_HEAD guard
      lets the coder resume resolving exactly where it left off. Only once
      every file is clean does `git commit --no-edit` conclude the merge with
      git's own two-parent message; that commit always happens on this path
      -- even if `git status --porcelain` looks clean (e.g. a prior turn
      already staged the resolution before being interrupted) -- because
      concluding an in-progress merge is mandatory, not conditional on there
      being a fresh diff.
    - No merge in progress: the existing single-parent commit, skipped
      entirely when `git status --porcelain` reports no changes at all, so a
      turn that only left commentary (no file edits) doesn't fail on
      "nothing to commit".
    """

    def _node(state: CoderState) -> dict[str, Any]:
        cwd = state["cwd"]
        conflict_files = state.get("merge_conflict_files") or []
        merging = bool(conflict_files)

        if merging:
            unresolved = _unresolved_conflict_markers(run_command, cwd, conflict_files)
            if unresolved:
                raise CoderGraphError(f"unresolved conflict markers remain in: {', '.join(unresolved)}")

        _run(run_command, ["git", "add", "-A"], cwd)
        status = _run(run_command, ["git", "status", "--porcelain"], cwd)
        paths = _parse_porcelain_paths(status.stdout)

        committed = False
        if paths or merging:
            if merging:
                _run(run_command, ["git", "commit", "--no-edit"], cwd)
            else:
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
    tail = parse_structured_tail(state.get("dac_last_text") or "")
    if tail.title:
        return f"{issue_id}: {tail.title}"[:120]
    # Legacy fallback: no TITLE tail -> the DAC summary's first narration line.
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
        ahead = _run(
            run_command,
            ["git", "rev-list", "--count", f"origin/{base_branch}..HEAD"],
            cwd,
            check=False,
        )
        if ahead.returncode == 0 and ahead.stdout.strip() == "0":
            # Nothing to open a PR from: the turn made no commits (a blocked
            # or no-op turn). gh pr create would fail "No commits between..."
            # and the kernel would classify that crash as transient and
            # retry it five times (REF-26). An empty pr_url is the honest
            # result.
            return {"pr_url": ""}

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


def _coder_blocked_summary(reason: str) -> str:
    reason = (reason or "").strip()
    return f"coder blocked: {reason}" if reason else "coder blocked: no reason given"


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
    interrupt_payload = state.get("interrupt_payload")
    tail = parse_structured_tail(state.get("dac_last_text") or "")
    # Surface the STATUS/TITLE/HANDOFF tail's handoff note (decisions,
    # interfaces, gotchas for dependents) on every terminal result so the
    # dispatcher can post it as a Linear comment. Capped so a runaway section
    # can't bloat the comment; None when the coder produced no handoff.
    handoff = tail.handoff[:4000] or None
    common: dict[str, Any] = {
        "run_id": state["run_id"],
        "issue_id": state["issue_id"],
        "lane": Lane.coder,
        "thread_id": state["thread_id"],
        "turn_count": turn_count,
        "tokens": tokens,
        "handoff": handoff,
    }
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
    elif tail.status == "blocked":
        # The turn completed but the coder self-reported STATUS: blocked -- an
        # ambiguity/missing-credential it can't resolve on its own, needing a
        # human. Mapped to needs_input (a non-retrying block) with the reason
        # the run_DAC node stashed. route_after_dac already skipped
        # commit/push/open_PR, so there is never a pr_url here.
        result = WorkerResult(
            **common,
            outcome=Outcome.blocked,
            block_kind=BlockKind.needs_input,
            summary=_coder_blocked_summary(state.get("blocked_reason") or tail.blocked_reason),
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
    transcript: TranscriptWriter | None = None,
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

    `transcript` (default `None` = disabled) threads through to both DAC
    turns the same way `checkpointer` does: a construction-time value closed
    over by `make_run_dac`/`make_run_docs`, never carried on `CoderState`.

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
    graph.add_node("sync_base", make_sync_base(resolved_run_command))
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer, transcript))
    graph.add_node(
        "run_docs", make_run_docs(resolved_docs_profile, agent_factory, turn_driver, checkpointer, transcript)
    )
    graph.add_node("commit", make_commit(resolved_run_command))
    graph.add_node("push", make_push(resolved_run_command))
    graph.add_node("open_PR", make_open_pr(resolved_run_command))
    graph.add_node("emit_result", emit_result)

    graph.add_edge(START, "load_context")
    graph.add_edge("load_context", "ensure_worktree")
    graph.add_edge("ensure_worktree", "sync_base")
    graph.add_edge("sync_base", "run_DAC")
    graph.add_conditional_edges("run_DAC", route_after_dac)
    graph.add_edge("run_docs", "commit")
    graph.add_edge("commit", "push")
    graph.add_edge("push", "open_PR")
    graph.add_edge("open_PR", "emit_result")
    graph.add_edge("emit_result", END)

    return graph.compile(checkpointer=checkpointer)
