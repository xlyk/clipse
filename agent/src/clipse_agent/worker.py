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

Only the Coder lane is wired to a real graph (Phase 2). Any other lane --
a real `Lane` member the kernel doesn't dispatch here yet (reviewer /
git_operator / scribe), or a garbage string that isn't a Lane member at all
-- is handled the same way: outcome=blocked, block_kind=transient, no graph
ever built or run.
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
from clipse_agent.graphs.coder import CoderState, build_coder_graph

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


async def _run_coder(args: argparse.Namespace) -> WorkerResult:
    """Drive one turn of the Coder graph and return its result.

    The checkpointer is built from --checkpoint-db when the kernel gave one
    (design doc: one checkpointer database per issue, path owned by the
    kernel); otherwise the graph runs with no checkpointer at all -- still
    correct for a single turn, just without cross-process resume.
    """
    input_state: CoderState = {
        "issue_id": args.issue,
        "run_id": args.run,
        "thread_id": args.thread,
        "workspace": args.workspace,
        "max_tokens": _resolve_max_tokens(args),
    }
    config: dict[str, Any] = {"configurable": {"thread_id": args.thread}}

    if not args.checkpoint_db:
        graph = build_coder_graph(checkpointer=None)
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]

    async with AsyncSqliteSaver.from_conn_string(args.checkpoint_db) as checkpointer:
        graph = build_coder_graph(checkpointer=checkpointer)
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]


async def _dispatch(args: argparse.Namespace) -> WorkerResult:
    """Route to the named lane's graph. Only "coder" is implemented; any
    other Lane member, or a string that isn't a Lane member at all, is
    reported as blocked/transient without building or running a graph.
    """
    lane = _coerce_lane(args.lane)
    if lane == Lane.coder:
        return await _run_coder(args)
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
        result = _blocked_transient(args, lane=Lane.coder, summary=f"clipse-worker internal error: {exc}")

    # exclude_none: optional fields (block_kind, pr_url) must be OMITTED when
    # unset, never emitted as null -- see amendment X2's "present iff" invariant.
    print(result.model_dump_json(by_alias=True, exclude_none=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
