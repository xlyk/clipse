"""clipse-worker entrypoint.

Phase 0 stub: parses the CLI args the dispatcher will invoke it with,
builds a schema-valid WorkerResult with benign placeholder values, and
prints it as a single line of JSON on stdout. Real graph execution
(LangGraph + Deep Agents Code) lands in a later phase.
"""

import argparse
import sys
from collections.abc import Sequence

from clipse_agent.contract import Tokens, WorkerResult


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="clipse-worker")
    parser.add_argument("--issue", default="")
    parser.add_argument("--lane", default="coder")
    parser.add_argument("--run", default="")
    parser.add_argument("--thread", default="")
    parser.add_argument("--workspace", default="")
    parser.add_argument("--scenario", default="")
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)

    result = WorkerResult(
        run_id=args.run,
        issue_id=args.issue,
        lane=args.lane,
        outcome="blocked",
        block_kind=None,
        summary="clipse-worker stub",
        artifacts=[],
        thread_id=args.thread,
        turn_count=0,
        tokens=Tokens(**{"in": 0, "out": 0}),
    )

    print(result.model_dump_json(by_alias=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
