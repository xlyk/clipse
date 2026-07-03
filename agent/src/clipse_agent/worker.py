"""clipse-worker entrypoint.

Parses the CLI flags the dispatcher's Spawner invokes a worker with
(`internal/spawn.workerArgs`: --issue/--lane/--run/--thread/--workspace,
plus --checkpoint-db/--max-tokens whenever the kernel has them configured --
see internal/spawn/local.go), dispatches to the named lane's LangGraph
graph, and ALWAYS prints exactly one line of schema-valid
`contract.WorkerResult` JSON to stdout -- even when something inside the
graph raises. The dispatcher parses only stdout; anything diagnostic
(tracebacks, etc) goes to stderr, which the kernel already redirects to a
per-issue log file rather than reading itself.

The Coder, Reviewer, and Scribe lanes are wired to real graphs. The
Git-operator lane is a real `Lane` member the kernel deliberately never
dispatches here: per decision O/J amendment, it runs as deterministic Go
(`internal/gitops`), never a DAC worker -- so it, and any garbage string
that isn't a Lane member at all, are handled the same way: outcome=blocked,
block_kind=transient, no graph ever built or run.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import sys
import traceback
from collections.abc import Sequence
from typing import Any

from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver

from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.graphs.coder import build_coder_graph
from clipse_agent.graphs.reviewer import build_reviewer_graph
from clipse_agent.graphs.scribe import build_scribe_graph

_ZERO_TOKENS = Tokens(**{"in": 0, "out": 0})

# The env fallback for --max-tokens (see _resolve_max_tokens), named after
# the same CLIPSE_ prefix as CLIPSE_ISSUE_TEXT (graphs/coder.py) and
# CLIPSE_CONFIG (cli/dispatch.go).
_MAX_TOKENS_ENV_VAR = "CLIPSE_MAX_TOKENS"


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="clipse-worker")
    parser.add_argument("--issue", default="")
    parser.add_argument("--lane", default="coder")
    parser.add_argument("--run", default="")
    parser.add_argument("--thread", default="")
    parser.add_argument("--workspace", default="")
    parser.add_argument("--checkpoint-db", default="")
    parser.add_argument("--max-tokens", type=int, default=None)
    return parser


def _resolve_max_tokens(args: argparse.Namespace) -> int | None:
    """--max-tokens wins when the kernel passed it; otherwise fall back to
    CLIPSE_MAX_TOKENS (the kernel always passes the flag when
    config.MaxTokensPerRun is positive -- see internal/spawn.workerArgs --
    so the env path exists for direct/manual invocations outside the
    Spawner). A missing or non-integer env value means "no ceiling", same
    as never setting either.
    """
    if args.max_tokens is not None:
        return args.max_tokens
    raw = os.environ.get(_MAX_TOKENS_ENV_VAR)
    if not raw:
        return None
    try:
        return int(raw)
    except ValueError:
        return None


def _coerce_lane(raw: str) -> Lane | None:
    try:
        return Lane(raw)
    except ValueError:
        return None


def _blocked_transient(args: argparse.Namespace, *, lane: Lane, summary: str) -> WorkerResult:
    """Build a schema-valid blocked/transient result from nothing but
    `args` and an already-valid `lane`. This must never itself raise --
    both the "lane not dispatched" branch and the top-level catch-all in
    `main` funnel through here, so a `WorkerResult` always comes out valid
    even when `args.lane` isn't a real Lane member at all.
    """
    return WorkerResult(
        run_id=args.run,
        issue_id=args.issue,
        lane=lane,
        outcome=Outcome.blocked,
        block_kind=BlockKind.transient,
        summary=summary,
        artifacts=[],
        thread_id=args.thread,
        turn_count=0,
        tokens=_ZERO_TOKENS,
    )


async def _run_lane_graph(args: argparse.Namespace, build_graph: Any, *, lane: Lane) -> WorkerResult:
    """Drive one turn of `build_graph`'s graph and return its result.

    Shared by every lane `_dispatch` knows how to route to (coder/reviewer/
    scribe): every lane's graph is invoked with the exact same input-state
    shape (issue_id/run_id/thread_id/workspace/max_tokens -- issue_text is
    covered separately, by the dispatcher's own $CLIPSE_ISSUE_TEXT env
    injection that every lane's `load_context` already falls back to) and
    the exact same checkpointer wiring; only which graph-builder function
    to call differs, and `_dispatch` passes that in already resolved from
    this module's own globals (see its docstring on why that matters for
    monkeypatching). The checkpointer is built from --checkpoint-db when the
    kernel gave one (design doc: one checkpointer database per issue, path
    owned by the kernel, shared by every lane that runs against that
    issue); otherwise the graph runs with no checkpointer at all -- still
    correct for a single turn, just without cross-process resume.

    `lane` is `_dispatch`'s own already-matched Lane for this call, used
    ONLY to namespace the OUTER wrapping-graph's checkpoint thread_id below
    -- a fresh claim passes the identical raw `--thread` regardless of
    lane, so coder/reviewer/scribe runs against the same issue would
    otherwise collide on this wrapping graph's own checkpoint the moment
    they shared a physical checkpointer (design doc: "one checkpointer
    database per issue", not one per lane). This mirrors each lane graph's
    own inner-DAC-thread namespace one level down (graphs/coder.py's
    `_DAC_THREAD_NAMESPACE_SUFFIX` == "::dac", and the reviewer/scribe
    analogues) -- same problem, same fix, one layer higher. `input_state`'s
    own "thread_id" stays the raw, un-namespaced value: it flows straight
    through to the emitted WorkerResult.thread_id, which the dispatcher
    reuses verbatim as `--thread` on a later continuation spawn, so it must
    never carry this module's internal namespacing.
    """
    input_state: dict[str, Any] = {
        "issue_id": args.issue,
        "run_id": args.run,
        "thread_id": args.thread,
        "workspace": args.workspace,
        "max_tokens": _resolve_max_tokens(args),
    }
    config: dict[str, Any] = {"configurable": {"thread_id": f"{args.thread}::{lane.value}"}}

    if not args.checkpoint_db:
        graph = build_graph(checkpointer=None)
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]

    async with AsyncSqliteSaver.from_conn_string(args.checkpoint_db) as checkpointer:
        graph = build_graph(checkpointer=checkpointer)
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]


async def _dispatch(args: argparse.Namespace) -> WorkerResult:
    """Route to the named lane's graph. "coder"/"reviewer"/"scribe" are
    implemented; any other Lane member (today: only "git_operator", which
    by design never gets a Python graph -- see the module docstring), or a
    string that isn't a Lane member at all, is reported as blocked/transient
    without building or running a graph.

    Each branch below references its `build_*_graph` function as a bare
    module-global name, resolved right here at call time rather than via
    some table built once at import time -- that is what lets a test's
    `monkeypatch.setattr(worker, "build_coder_graph", fake)` (etc) actually
    take effect: a dict of function objects assembled at import time would
    freeze in the *original* functions before any test could ever replace
    them.
    """
    lane = _coerce_lane(args.lane)
    if lane == Lane.coder:
        return await _run_lane_graph(args, build_coder_graph, lane=Lane.coder)
    if lane == Lane.reviewer:
        return await _run_lane_graph(args, build_reviewer_graph, lane=Lane.reviewer)
    if lane == Lane.scribe:
        return await _run_lane_graph(args, build_scribe_graph, lane=Lane.scribe)
    return _blocked_transient(
        args,
        lane=lane if lane is not None else Lane.coder,
        summary=f"clipse-worker: lane {args.lane!r} is not implemented",
    )


def main(argv: Sequence[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)

    try:
        result = asyncio.run(_dispatch(args))
    except Exception as exc:  # noqa: BLE001 - the whole point: a worker must never die with no result
        traceback.print_exc(file=sys.stderr)
        # Mirrors _dispatch's own fallback: report whichever lane was
        # actually requested when it's a real Lane member, rather than
        # always claiming Lane.coder regardless of --lane.
        result = _blocked_transient(
            args, lane=_coerce_lane(args.lane) or Lane.coder, summary=f"clipse-worker internal error: {exc}"
        )

    # exclude_none: optional fields (block_kind, pr_url) must be OMITTED when
    # unset, never emitted as null -- see amendment X2's "present iff" invariant.
    print(result.model_dump_json(by_alias=True, exclude_none=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
