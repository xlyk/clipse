"""LangGraph StateGraph for the Reviewer lane.

Wraps `clipse_agent.dac` (the DAC engine wrapper) in a small graph, analogous
to `graphs.coder`'s, giving the kernel a typed, testable seam around one
review turn, per the design doc's Reviewer lane ("Check out the PR, review;
return `pass` or `changes_requested` + inline comments"):

    load_context -> ensure_worktree -> run_DAC -> classify -> {
        PASS               -> emit_result (done)
        CHANGES_REQUESTED  -> post_comments -> emit_result (changes_requested)
        interrupted / over token ceiling -> emit_result (blocked)
    }

`run_DAC` is the only async node (it awaits `dac.drive_turn`), so the
compiled graph must always be driven with `.ainvoke`/`.astream`, never the
sync `.invoke` -- same constraint as `graphs.coder`, for the same reason
(LangGraph raises `TypeError` from a sync `.invoke` the moment it reaches an
async-only node).

Outcome mapping (design doc "Board & state machine", `internal/board.go`'s
transition table): the Reviewer lane only ever runs while a card sits in the
`review` column, and `internal/board.Next` only defines `done` (-> Merging),
`changes_requested` (-> Rework), and `blocked` (-> Blocked) as legal
outcomes from there (see `TestNext_AllPairsCovered` in
`internal/board/board_test.go`). This module never emits `needs_review` or
`continue` -- those belong to the Coder lane's own columns.

This lane is **advisory only**: `internal/gitops`'s CI + branch-protection
check is the authoritative merge gate (design doc, "Threat model" and
decision D2/J) -- a `done` verdict here only hands the card to Merging, it
never merges anything itself, and it is never sufficient alone even when it
does pass.

Reuse of `graphs.coder`: `CommandResult`/`CommandRunner` (the subprocess
seam), `load_context`, and `make_ensure_worktree` are genuinely
lane-agnostic in `graphs.coder` -- reused here verbatim rather than
duplicated (see each import site below for why). `make_run_dac` is
deliberately **not** reused verbatim: see its own docstring below for the
cross-lane checkpoint-thread collision that would otherwise cause.

Safety: this module never calls `deepagents_code` directly -- it only calls
`dac.build_coder_agent` / `dac.drive_turn` (the same two entry points
`graphs.coder` uses), which already enforce the non-negotiable
`auto_approve=False, interrupt_shell_only=True` shell wiring (see dac.py's
module docstring). `dac.build_coder_agent` is named after its original
caller but is not Coder-specific in its implementation -- it only reads
`profile.model` / `.assistant_id` / `.system_prompt` / `.shell_allow_list`,
so passing it this lane's `ReviewerProfile` reuses that exact safety wiring
rather than re-deriving (and risking drift from) it.
"""

from __future__ import annotations

import json
import re
import subprocess
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, TypedDict

from langgraph.graph import END, START, StateGraph

from clipse_agent import dac
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.graphs import coder
from clipse_agent.graphs.coder import CoderGraphError as ReviewerGraphError
from clipse_agent.profiles.reviewer import ReviewerProfile, get_reviewer_profile

if TYPE_CHECKING:
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.graph.state import CompiledStateGraph

__all__ = [
    "ReviewerGraphError",
    "InlineComment",
    "ReviewerState",
    "load_context",
    "classify",
    "route_after_dac",
    "route_after_classify",
    "emit_result",
    "make_run_dac",
    "make_post_comments",
    "build_reviewer_graph",
]

# `load_context` and `make_ensure_worktree` have zero Coder-specific logic
# (pure issue-text composition; filesystem + `git rev-parse` validation) --
# reused verbatim rather than duplicated. Re-exported in __all__ so callers
# can reach them as `reviewer.load_context` without reaching into
# `graphs.coder` themselves.
load_context = coder.load_context

# The real DAC-agent builder / turn driver this graph calls by default.
# Injectable so tests never build a real DAC agent or drive a real model --
# same seam `graphs.coder` uses, over this lane's own ReviewerProfile.
AgentFactory = Callable[[ReviewerProfile, "BaseCheckpointSaver | None", str], tuple[Any, Any]]
# Fully lane-agnostic already (no CoderProfile/ReviewerProfile in its own
# signature) -- reused as-is.
TurnDriver = coder.TurnDriver


# ---------------------------------------------------------------------------
# Subprocess seam (git/gh) -- reuses graphs.coder's CommandResult/
# CommandRunner types (the "subprocess seam"); `_run`/`_default_run_command`
# are tiny and kept lane-local so a Reviewer-lane infra failure raises
# clearly attributed to this module, not a private symbol reached into
# graphs.coder.
# ---------------------------------------------------------------------------

CommandResult = coder.CommandResult
CommandRunner = coder.CommandRunner


def _default_run_command(argv: Sequence[str], cwd: str) -> CommandResult:
    proc = subprocess.run(list(argv), cwd=cwd, capture_output=True, text=True)
    return CommandResult(returncode=proc.returncode, stdout=proc.stdout, stderr=proc.stderr)


def _run(run_command: CommandRunner, argv: Sequence[str], cwd: str, *, check: bool = True) -> CommandResult:
    result = run_command(argv, cwd)
    if check and result.returncode != 0:
        raise ReviewerGraphError(
            f"command failed (exit {result.returncode}): {' '.join(argv)}\nstderr: {result.stderr}"
        )
    return result


# ---------------------------------------------------------------------------
# Graph state
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class InlineComment:
    """One structured review finding, parsed from the DAC turn's final
    message: a file/line pair plus the comment text `post_comments` attaches
    there as a real inline PR review comment.
    """

    path: str
    line: int
    body: str


class ReviewerState(TypedDict, total=False):
    """State threaded through the Reviewer graph for one review turn.

    Every key is optional at the TypedDict level (`total=False`) -- e.g. the
    blocked path never reaches `review_comments`/`pr_url`. Mirrors
    `graphs.coder.CoderState`'s shape closely by design: both lanes run
    against the *same* worktree/workspace for a given issue, and sharing
    field names (`branch`, `cwd`, `dac_summary`, `prior_summary`, ...) is
    what lets this graph reuse `load_context`/`make_ensure_worktree`
    unmodified.
    """

    # --- Supplied by the caller (worker.py) at invocation ---
    issue_id: str
    run_id: str
    thread_id: str
    workspace: str
    branch: str  # if omitted, ensure_worktree derives it via `git rev-parse`
    issue_text: str  # falls back to $CLIPSE_ISSUE_TEXT if omitted
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

    # --- classify ---
    review_passed: bool
    review_comments: list[InlineComment]

    # --- post_comments ---
    pr_url: str | None
    comments_posted: int

    # --- emit_result ---
    result: WorkerResult


# Namespaces this graph's own inner DAC checkpoint thread away from BOTH (a)
# this wrapping graph's own outer checkpoint thread, and (b) the Coder
# lane's inner DAC thread (graphs.coder._DAC_THREAD_NAMESPACE_SUFFIX ==
# "::dac"). The two lanes' runs for one issue share one physical checkpoint
# DB file (design doc: "one checkpointer database per issue", issue-scoped,
# not lane-scoped) and, on a fresh dispatch, the *same* outer thread_id too
# (the kernel passes "" for every fresh claim regardless of lane) -- so
# without a lane-distinct suffix here, this lane's DAC agent would resume
# the Coder's entire prior message history (a different system prompt, a
# different shell allow-list) as its own. A different suffix than Coder's
# is the fix: it guarantees the two lanes' DAC checkpoints never alias to
# the same storage key even when everything else about the invocation is
# identical.
_REVIEW_DAC_THREAD_NAMESPACE_SUFFIX = "::review-dac"


def _dac_config(thread_id: str) -> dict[str, Any]:
    return {"configurable": {"thread_id": f"{thread_id}{_REVIEW_DAC_THREAD_NAMESPACE_SUFFIX}"}}


# ---------------------------------------------------------------------------
# run_DAC
# ---------------------------------------------------------------------------


def make_run_dac(
    profile: ReviewerProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
) -> Callable[[ReviewerState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh `task_text` turn normally, or a
    `resume` of a previously-interrupted turn when `resume_payload` is set.

    Structurally identical to `graphs.coder.make_run_dac`, but intentionally
    **not** imported from there: it must call this module's own
    `_dac_config` (a distinct thread-namespace suffix), not Coder's -- see
    that function's docstring for the cross-lane checkpoint collision this
    prevents. Everything else about driving a DAC turn is genuinely
    lane-agnostic, hence the otherwise-identical body.
    """

    async def _node(state: ReviewerState) -> dict[str, Any]:
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


def route_after_dac(state: ReviewerState) -> str:
    """Pick the next node once `run_DAC` has run.

    A token-ceiling abort or a genuine interrupt both skip straight to
    `emit_result` as blocked (mirrors `graphs.coder.route_after_dac`);
    anything else proceeds to classify the review verdict.
    """
    if state.get("token_ceiling_exceeded") or state.get("interrupt_payload") is not None:
        return "emit_result"
    return "classify"


# ---------------------------------------------------------------------------
# classify
# ---------------------------------------------------------------------------

_VERDICT_PASS = "PASS"

_VERDICT_RE = re.compile(r"VERDICT:\s*(PASS|CHANGES_REQUESTED)\b", re.IGNORECASE)
_INLINE_COMMENT_RE = re.compile(r"^-\s*([^\s:]+):(\d+):\s*(.+)$")


def _find_verdict(dac_summary: str) -> re.Match[str] | None:
    """Return the *last* `VERDICT: ...` match in `dac_summary`, or None.

    The last match wins so that the system prompt's own protocol text (which
    literally contains the string "VERDICT:") being echoed or quoted earlier
    in the message is never mistaken for the model's actual decision -- that
    is always whatever it states last.
    """
    matches = list(_VERDICT_RE.finditer(dac_summary))
    return matches[-1] if matches else None


def _parse_inline_comments(dac_summary: str, start: int) -> list[InlineComment]:
    """Parse `- path:line: body` bullet lines appearing at or after `start`
    (the verdict match's end) into structured `InlineComment`s. Only text
    after the verdict is scanned, so a bullet line elsewhere in the model's
    own reasoning can never be mistaken for a review finding.
    """
    comments: list[InlineComment] = []
    for line in dac_summary[start:].splitlines():
        match = _INLINE_COMMENT_RE.match(line.strip())
        if match:
            path, line_no, body = match.groups()
            comments.append(InlineComment(path=path, line=int(line_no), body=body.strip()))
    return comments


def classify(state: ReviewerState) -> dict[str, Any]:
    """Decide PASS vs CHANGES_REQUESTED from this turn's DAC output.

    Conservative by design: only an explicit `VERDICT: PASS` line clears a
    PR. A missing or unparseable verdict -- the model rambled, or forgot the
    protocol -- is treated as CHANGES_REQUESTED with no comments, never as
    PASS. This lane is advisory-only (design doc: "reviewer pass is
    advisory input, never sufficient alone"), but the one thing it must
    never do is wrongly signal "safe to merge".
    """
    dac_summary = state.get("dac_summary") or ""
    verdict_match = _find_verdict(dac_summary)
    passed = verdict_match is not None and verdict_match.group(1).upper() == _VERDICT_PASS

    comments: list[InlineComment] = []
    if not passed and verdict_match is not None:
        comments = _parse_inline_comments(dac_summary, verdict_match.end())

    return {"review_passed": passed, "review_comments": comments}


def route_after_classify(state: ReviewerState) -> str:
    """A pass goes straight to `emit_result`; anything else must post its
    findings to the PR first."""
    return "emit_result" if state.get("review_passed") else "post_comments"


# ---------------------------------------------------------------------------
# post_comments
# ---------------------------------------------------------------------------


def make_post_comments(run_command: CommandRunner) -> Callable[[ReviewerState], dict[str, Any]]:
    """Post this turn's review findings to the PR.

    One inline `gh api` review comment per parsed `InlineComment` (GitHub's
    "create a review comment for a pull request" endpoint, which needs the
    PR's head commit + a file path + a line), plus a single `gh pr review
    --request-changes` carrying the overall summary. The summary review
    always runs -- even when the model gave no machine-parseable per-line
    findings, `review_comments` is simply empty and no inline comments are
    posted, but the PR must still show a changes-requested review.
    """

    def _node(state: ReviewerState) -> dict[str, Any]:
        branch = state["branch"]
        cwd = state["cwd"]
        comments: list[InlineComment] = state.get("review_comments") or []

        view = _run(run_command, ["gh", "pr", "view", branch, "--json", "number,headRefOid,url"], cwd)
        pr_info = json.loads(view.stdout)
        pr_number = pr_info["number"]
        commit_sha = pr_info["headRefOid"]

        for comment in comments:
            _run(
                run_command,
                [
                    "gh",
                    "api",
                    f"repos/{{owner}}/{{repo}}/pulls/{pr_number}/comments",
                    "-f",
                    f"body={comment.body}",
                    "-f",
                    f"commit_id={commit_sha}",
                    "-f",
                    f"path={comment.path}",
                    "-F",
                    f"line={comment.line}",
                    "-f",
                    "side=RIGHT",
                ],
                cwd,
            )

        _run(
            run_command,
            ["gh", "pr", "review", branch, "--request-changes", "--body", _changes_summary(state)],
            cwd,
        )

        return {"pr_url": pr_info.get("url"), "comments_posted": len(comments)}

    return _node


# ---------------------------------------------------------------------------
# emit_result
# ---------------------------------------------------------------------------


def _capability_summary(tokens_in: int, tokens_out: int) -> str:
    total = tokens_in + tokens_out
    return (
        f"Aborted: exceeded this run's token budget after spending {total} "
        f"tokens ({tokens_in} in / {tokens_out} out)."
    )


def _needs_input_summary(interrupt_payload: list[Any]) -> str:
    return f"DAC paused for input it can't resolve on its own: {interrupt_payload!r}"


def _pass_summary(state: ReviewerState) -> str:
    dac_summary = (state.get("dac_summary") or "").strip()
    return dac_summary or "Reviewed the diff: no issues found."


def _changes_summary(state: ReviewerState) -> str:
    dac_summary = (state.get("dac_summary") or "").strip()
    comments = state.get("review_comments") or []
    parts = [dac_summary] if dac_summary else []
    if comments:
        parts.append(f"Posted {len(comments)} inline comment(s).")
    return " ".join(parts) if parts else "Requested changes."


def emit_result(state: ReviewerState) -> dict[str, Any]:
    """Map this turn's outcome onto the shared `contract.WorkerResult`.

    From the `review` column, `internal/board.Next` only allows
    done/changes_requested/blocked (see `internal/board/board.go`'s
    transition table) -- this always produces exactly one of those three,
    never `needs_review`/`continue`, which belong to the Coder lane's own
    columns. A token-ceiling abort takes priority over an interrupt (same
    precedence as `graphs.coder.emit_result`), and both map to `blocked`
    with a distinct `block_kind` -- present in every blocked branch,
    consistent with amendment X2's "present iff outcome == blocked"
    invariant.

    Also returns `prior_summary`: whatever DAC said this turn, threaded
    forward exactly like `graphs.coder.emit_result` does, so a later turn on
    the same checkpointed thread sees it via `load_context` without the
    caller having to resupply it.
    """
    tokens_in = state.get("tokens_in", 0)
    tokens_out = state.get("tokens_out", 0)
    tokens = Tokens(**{"in": tokens_in, "out": tokens_out})
    turn_count = state.get("turn_count", 0) + 1
    common: dict[str, Any] = {
        "run_id": state["run_id"],
        "issue_id": state["issue_id"],
        "lane": Lane.reviewer,
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
            artifacts=[],
        )
    elif interrupt_payload is not None:
        result = WorkerResult(
            **common,
            outcome=Outcome.blocked,
            block_kind=BlockKind.needs_input,
            summary=_needs_input_summary(interrupt_payload),
            artifacts=[],
        )
    elif state.get("review_passed"):
        result = WorkerResult(
            **common,
            outcome=Outcome.done,
            summary=_pass_summary(state),
            artifacts=[],
            pr_url=state.get("pr_url"),
        )
    else:
        result = WorkerResult(
            **common,
            outcome=Outcome.changes_requested,
            summary=_changes_summary(state),
            artifacts=[],
            pr_url=state.get("pr_url"),
        )

    return {
        "result": result,
        "prior_summary": state.get("dac_summary", ""),
        "turn_count": turn_count,
    }


# ---------------------------------------------------------------------------
# Graph assembly
# ---------------------------------------------------------------------------


def build_reviewer_graph(
    *,
    checkpointer: BaseCheckpointSaver | None = None,
    profile: ReviewerProfile | None = None,
    agent_factory: AgentFactory = dac.build_coder_agent,
    turn_driver: TurnDriver = dac.drive_turn,
    run_command: CommandRunner | None = None,
) -> CompiledStateGraph[Any, Any, Any, Any]:
    """Build and compile the Reviewer lane's graph.

    `agent_factory` defaults to `dac.build_coder_agent` -- genuinely
    lane-agnostic despite its name (see the module docstring) -- and
    `turn_driver` to `dac.drive_turn`; `run_command` defaults to a real
    `subprocess.run`-backed runner. Tests override all three so no real
    model, DAC agent, subprocess, or network call is ever touched.

    Compiled with `checkpointer` (LangGraph resume support), which the same
    call also forwards into `agent_factory` so DAC's own agent shares one
    physical checkpoint store with this wrapping graph (design doc: "one
    checkpointer database per issue") -- see `_dac_config` for how this
    lane's own DAC sub-thread avoids colliding with the Coder lane's within
    that shared store.

    The returned graph's only async node is `run_DAC`, so it must be driven
    with `.ainvoke`/`.astream` -- never the sync `.invoke`.
    """
    resolved_profile = profile if profile is not None else get_reviewer_profile()
    resolved_run_command = run_command if run_command is not None else _default_run_command

    graph: StateGraph[ReviewerState, Any, Any, Any] = StateGraph(ReviewerState)
    graph.add_node("load_context", load_context)
    graph.add_node("ensure_worktree", coder.make_ensure_worktree(resolved_run_command))
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer))
    graph.add_node("classify", classify)
    graph.add_node("post_comments", make_post_comments(resolved_run_command))
    graph.add_node("emit_result", emit_result)

    graph.add_edge(START, "load_context")
    graph.add_edge("load_context", "ensure_worktree")
    graph.add_edge("ensure_worktree", "run_DAC")
    graph.add_conditional_edges("run_DAC", route_after_dac)
    graph.add_conditional_edges("classify", route_after_classify)
    graph.add_edge("post_comments", "emit_result")
    graph.add_edge("emit_result", END)

    return graph.compile(checkpointer=checkpointer)
