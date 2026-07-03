"""LangGraph StateGraph for the Scribe lane.

Wraps `clipse_agent.dac` (the DAC engine wrapper) in a small graph, analogous
to `graphs.coder`'s and `graphs.reviewer`'s, giving the kernel a typed,
testable seam around one documentation turn, per the design doc's Scribe
lane ("Documentation. Runs on every merged issue; no-ops when there is
nothing to write."):

    load_context -> ensure_worktree -> run_DAC -> commit -> {
        wrote docs -> push -> open_PR -> emit_result (done)
        no-op      -> emit_result (done, no PR)
    }
    interrupted / over token ceiling -> emit_result (blocked)

`run_DAC` is the only async node (it awaits `dac.drive_turn`), so the
compiled graph must always be driven with `.ainvoke`/`.astream`, never the
sync `.invoke` -- same constraint as `graphs.coder`/`graphs.reviewer`, for
the same reason (LangGraph raises `TypeError` from a sync `.invoke` the
moment it reaches an async-only node).

Outcome mapping (design doc "Board & state machine", `internal/board.go`'s
transition table): the Scribe lane only ever runs while a card sits in the
`documentation` column, and `internal/board.Next` only defines `done` (->
Done) and `blocked` (-> Blocked) as legal outcomes from there (see
`TestNext_AllPairsCovered` in `internal/board/board_test.go`) -- there is no
"changes requested" or "needs review" concept for documentation. This module
therefore emits `done` whether it wrote docs or correctly decided a no-op is
right, never `changes_requested`/`needs_review`/`continue`.

Reuse of `graphs.coder`: `CommandResult`/`CommandRunner`/`TurnDriver` (the
subprocess seam + turn-driver type), `load_context`, `make_ensure_worktree`,
`make_commit`, and `make_push` are genuinely lane-agnostic in `graphs.coder`
-- none of them reference anything Coder-specific, they only read/write the
generic state keys every lane's graph shares (`branch`, `cwd`, `issue_id`,
`dac_summary`, ...) -- so they are reused here verbatim rather than
duplicated, exactly as `graphs.reviewer` already reuses `load_context`/
`make_ensure_worktree`. This lane is the second real use case for
`make_commit`/`make_push` (the Reviewer lane never commits), which is what
earns reusing rather than copying them. `make_run_dac` is deliberately
**not** reused verbatim: see its own docstring below for the cross-lane
checkpoint-thread collision that would otherwise cause. `make_open_pr` is
also **not** reused: Coder's version always opens a draft (the Reviewer
lane is expected to un-draft it), but no lane ever reviews a documentation
PR (`internal/board.Next` sends `documentation` straight to `done`/
`blocked`, never a review column) -- a draft with no one to ever un-draft it
would sit forever, so this lane's own `make_open_pr` opens its PR ready, not
draft.

Safety: this module never calls `deepagents_code` directly -- it only calls
`dac.build_coder_agent` / `dac.drive_turn` (the same two entry points
`graphs.coder`/`graphs.reviewer` use), which already enforce the
non-negotiable `auto_approve=False, interrupt_shell_only=True` shell wiring
(see dac.py's module docstring). `dac.build_coder_agent` is named after its
original caller but is not Coder-specific in its implementation -- it only
reads `profile.model` / `.assistant_id` / `.system_prompt` /
`.shell_allow_list`, so passing it this lane's `ScribeProfile` reuses that
exact safety wiring rather than re-deriving (and risking drift from) it.
"""

from __future__ import annotations

import json
import subprocess
from collections.abc import Awaitable, Callable, Sequence
from typing import TYPE_CHECKING, Any, TypedDict

from langgraph.graph import END, START, StateGraph

from clipse_agent import dac
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.graphs import coder
from clipse_agent.graphs.coder import CoderGraphError as ScribeGraphError
from clipse_agent.profiles.scribe import ScribeProfile, get_scribe_profile

if TYPE_CHECKING:
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.graph.state import CompiledStateGraph

__all__ = [
    "ScribeGraphError",
    "ScribeState",
    "load_context",
    "route_after_dac",
    "route_after_commit",
    "emit_result",
    "make_run_dac",
    "make_open_pr",
    "build_scribe_graph",
]

# Zero lane-specific logic in any of these -- reused verbatim rather than
# duplicated (see the module docstring for why each is safe to share).
load_context = coder.load_context
CommandResult = coder.CommandResult
CommandRunner = coder.CommandRunner
TurnDriver = coder.TurnDriver

# The real DAC-agent builder / turn driver this graph calls by default.
# Injectable so tests never build a real DAC agent or drive a real model --
# same seam `graphs.coder`/`graphs.reviewer` use, over this lane's own
# ScribeProfile.
AgentFactory = Callable[[ScribeProfile, "BaseCheckpointSaver | None", str], tuple[Any, Any]]


# ---------------------------------------------------------------------------
# Subprocess seam (git/gh) -- kept lane-local (own `_run`/
# `_default_run_command`) for the one node genuinely unique to this lane
# (`make_open_pr`), so an infra failure there is attributed to *this*
# module -- mirrors `graphs.reviewer`'s identical choice for
# `make_post_comments`. `make_ensure_worktree`/`make_commit`/`make_push`,
# reused verbatim below, keep using `graphs.coder`'s own internal `_run`
# (aliased to the same `ScribeGraphError` class, so nothing is lost).
# ---------------------------------------------------------------------------


def _default_run_command(argv: Sequence[str], cwd: str) -> CommandResult:
    proc = subprocess.run(list(argv), cwd=cwd, capture_output=True, text=True)
    return CommandResult(returncode=proc.returncode, stdout=proc.stdout, stderr=proc.stderr)


def _run(run_command: CommandRunner, argv: Sequence[str], cwd: str, *, check: bool = True) -> CommandResult:
    result = run_command(argv, cwd)
    if check and result.returncode != 0:
        raise ScribeGraphError(
            f"command failed (exit {result.returncode}): {' '.join(argv)}\nstderr: {result.stderr}"
        )
    return result


# ---------------------------------------------------------------------------
# Graph state
# ---------------------------------------------------------------------------


class ScribeState(TypedDict, total=False):
    """State threaded through the Scribe graph for one documentation turn.

    Every key is optional at the TypedDict level (`total=False`) -- e.g. the
    no-op path never reaches `pr_url`. Mirrors `graphs.coder.CoderState`'s
    shape closely by design: sharing field names (`branch`, `cwd`,
    `dac_summary`, `prior_summary`, ...) is what lets this graph reuse
    `load_context`/`make_ensure_worktree`/`make_commit`/`make_push`
    unmodified.
    """

    # --- Supplied by the caller (worker.py) at invocation ---
    issue_id: str
    run_id: str
    thread_id: str
    workspace: str
    branch: str  # if omitted, ensure_worktree derives it via `git rev-parse`
    base_branch: str  # base branch for a newly-created PR; default "main"
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

    # --- commit (reused from graphs.coder) ---
    artifacts: list[str]
    committed: bool

    # --- open_PR ---
    pr_url: str | None

    # --- emit_result ---
    result: WorkerResult


# Namespaces this graph's own inner DAC checkpoint thread away from every
# other lane's (graphs.coder's "::dac", graphs.reviewer's "::review-dac"),
# and from this wrapping graph's own outer checkpoint thread. All three
# lanes share one physical checkpoint DB per issue and, on a fresh dispatch,
# the same outer thread_id too -- see graphs.reviewer's identical-purpose
# `_REVIEW_DAC_THREAD_NAMESPACE_SUFFIX` docstring for exactly which
# cross-lane collision this prevents.
_SCRIBE_DAC_THREAD_NAMESPACE_SUFFIX = "::scribe-dac"


def _dac_config(thread_id: str) -> dict[str, Any]:
    return {"configurable": {"thread_id": f"{thread_id}{_SCRIBE_DAC_THREAD_NAMESPACE_SUFFIX}"}}


# ---------------------------------------------------------------------------
# run_DAC
# ---------------------------------------------------------------------------


def make_run_dac(
    profile: ScribeProfile,
    agent_factory: AgentFactory,
    turn_driver: TurnDriver,
    checkpointer: BaseCheckpointSaver | None,
) -> Callable[[ScribeState], Awaitable[dict[str, Any]]]:
    """Drive exactly one DAC turn: a fresh `task_text` turn normally, or a
    `resume` of a previously-interrupted turn when `resume_payload` is set.

    Structurally identical to `graphs.coder.make_run_dac`/`graphs.reviewer.
    make_run_dac`, but intentionally not imported from either: it must call
    this module's own `_dac_config` (a distinct thread-namespace suffix),
    not theirs -- see that function's docstring for the cross-lane
    checkpoint collision this prevents. Everything else about driving a DAC
    turn is genuinely lane-agnostic, hence the otherwise-identical body.
    """

    async def _node(state: ScribeState) -> dict[str, Any]:
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


def route_after_dac(state: ScribeState) -> str:
    """Pick the next node once `run_DAC` has run.

    A token-ceiling abort or a genuine interrupt both skip straight to
    `emit_result` as blocked (mirrors `graphs.coder`/`graphs.reviewer`'s
    identical rule); anything else proceeds to `commit` -- the same
    deterministic idempotent safety net the Coder lane uses, so a doc edit
    DAC made (or correctly didn't make) this turn is captured regardless of
    whether DAC itself also tried to commit it.
    """
    if state.get("token_ceiling_exceeded") or state.get("interrupt_payload") is not None:
        return "emit_result"
    return "commit"


def route_after_commit(state: ScribeState) -> str:
    """A no-op turn (nothing staged) skips `push`/`open_PR` entirely and
    goes straight to `emit_result`.

    Unlike the Coder lane -- whose PR already exists from an earlier turn,
    so re-pushing/re-viewing it is always worth doing even on a quiet turn
    -- a fresh Scribe turn with nothing committed has no branch worth
    pushing, and must never open an empty PR just because it ran (design
    doc: "no-ops when there is nothing to write").
    """
    return "push" if state.get("committed") else "emit_result"


# ---------------------------------------------------------------------------
# open_PR
# ---------------------------------------------------------------------------


def _pr_title(state: ScribeState) -> str:
    issue_id = state.get("issue_id", "")
    lines = (state.get("dac_summary") or "").strip().splitlines()
    headline = lines[0] if lines else "update docs"
    return f"docs({issue_id}): {headline}"[:120]


def _pr_body(state: ScribeState) -> str:
    issue_id = state.get("issue_id", "")
    summary = (state.get("dac_summary") or "").strip()
    body = f"Documentation for {issue_id}."
    if summary:
        body += f"\n\n{summary}"
    return body


def make_open_pr(run_command: CommandRunner) -> Callable[[ScribeState], dict[str, Any]]:
    """Idempotently open (or reuse) this lane's own docs PR.

    `gh pr view <branch> --json url` first, exactly like `graphs.coder.
    make_open_pr` -- a crash after a previous turn's push-but-before-record
    reuses the existing PR instead of opening a second one. Unlike Coder's,
    this never passes `--draft`: no lane ever reviews a documentation PR
    (`internal/board.Next` sends `documentation` straight to `done`/
    `blocked`), so a draft here would have no one to ever take it out of
    draft.
    """

    def _node(state: ScribeState) -> dict[str, Any]:
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
        # stdout on success (it has no --json flag -- same observation
        # graphs.coder.make_open_pr's docstring records against the
        # installed gh CLI's own --help).
        url = next((line.strip() for line in reversed(created.stdout.splitlines()) if line.strip()), "")
        return {"pr_url": url}

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


def _done_summary(state: ScribeState) -> str:
    dac_summary = (state.get("dac_summary") or "").strip()
    if dac_summary:
        return dac_summary
    if state.get("committed"):
        return "Updated documentation."
    return "No documentation changes needed for this merge."


def emit_result(state: ScribeState) -> dict[str, Any]:
    """Map this turn's outcome onto the shared `contract.WorkerResult`.

    From the `documentation` column, `internal/board.Next` only allows
    done/blocked (see `internal/board/board.go`'s transition table) -- this
    always produces exactly one of those two, never `changes_requested`/
    `needs_review`/`continue`. A clean DAC turn always emits `done`,
    whether or not it actually wrote anything (design doc: "no-ops when
    there is nothing to write" is the expected common case, not a
    failure). A token-ceiling abort takes priority over an interrupt (same
    precedence as `graphs.coder`/`graphs.reviewer`), and both map to
    `blocked` with a distinct `block_kind` -- present in every blocked
    branch, consistent with amendment X2's "present iff outcome ==
    blocked" invariant.

    Also returns `prior_summary`: whatever DAC said this turn, threaded
    forward exactly like the other lanes' `emit_result`, so a later turn on
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
        "lane": Lane.scribe,
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
            outcome=Outcome.done,
            summary=_done_summary(state),
            artifacts=state.get("artifacts", []),
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


def build_scribe_graph(
    *,
    checkpointer: BaseCheckpointSaver | None = None,
    profile: ScribeProfile | None = None,
    agent_factory: AgentFactory = dac.build_coder_agent,
    turn_driver: TurnDriver = dac.drive_turn,
    run_command: CommandRunner | None = None,
) -> CompiledStateGraph[Any, Any, Any, Any]:
    """Build and compile the Scribe lane's graph.

    `agent_factory` defaults to `dac.build_coder_agent` -- genuinely
    lane-agnostic despite its name (see the module docstring) -- and
    `turn_driver` to `dac.drive_turn`; `run_command` defaults to a real
    `subprocess.run`-backed runner. Tests override all three so no real
    model, DAC agent, subprocess, or network call is ever touched.

    Compiled with `checkpointer` (LangGraph resume support), which the same
    call also forwards into `agent_factory` so DAC's own agent shares one
    physical checkpoint store with this wrapping graph (design doc: "one
    checkpointer database per issue") -- see `_dac_config` for how this
    lane's own DAC sub-thread avoids colliding with the Coder/Reviewer
    lanes' within that shared store.

    The returned graph's only async node is `run_DAC`, so it must be driven
    with `.ainvoke`/`.astream` -- never the sync `.invoke`.
    """
    resolved_profile = profile if profile is not None else get_scribe_profile()
    resolved_run_command = run_command if run_command is not None else _default_run_command

    graph: StateGraph[ScribeState, Any, Any, Any] = StateGraph(ScribeState)
    graph.add_node("load_context", load_context)
    graph.add_node("ensure_worktree", coder.make_ensure_worktree(resolved_run_command))
    graph.add_node("run_DAC", make_run_dac(resolved_profile, agent_factory, turn_driver, checkpointer))
    graph.add_node("commit", coder.make_commit(resolved_run_command))
    graph.add_node("push", coder.make_push(resolved_run_command))
    graph.add_node("open_PR", make_open_pr(resolved_run_command))
    graph.add_node("emit_result", emit_result)

    graph.add_edge(START, "load_context")
    graph.add_edge("load_context", "ensure_worktree")
    graph.add_edge("ensure_worktree", "run_DAC")
    graph.add_conditional_edges("run_DAC", route_after_dac)
    graph.add_conditional_edges("commit", route_after_commit)
    graph.add_edge("push", "open_PR")
    graph.add_edge("open_PR", "emit_result")
    graph.add_edge("emit_result", END)

    return graph.compile(checkpointer=checkpointer)
