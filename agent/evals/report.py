#!/usr/bin/env python3
"""Summarize the newest eval run: pass/fail, tokens, wall time per case.

Usage: uv run python evals/report.py [path/to/run.jsonl]
Defaults to results/latest.jsonl (the current run's symlink). Stdlib only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

_RESULTS_DIR = Path(__file__).parent / "results"


def _load(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def summarize(path: Path) -> str:
    rows = _load(path)
    tests: dict[str, dict] = {}
    for row in rows:
        entry = tests.setdefault(row["test"], {
            "status": "?", "duration_s": 0.0, "tokens_in": 0, "tokens_out": 0, "outcomes": [],
        })
        if "status" in row:
            entry["status"] = row["status"]
            entry["duration_s"] = row.get("duration_s", 0.0)
        if "outcome" in row:
            entry["outcomes"].append(row["outcome"])
            # L2 convergence rows carry loop_tokens_in/out -- the FULL
            # multi-round loop's tokens -- alongside tokens_in/out, which hold
            # only the final turn's tokens. Prefer the loop total so it is
            # counted once, not added on top of the (already-included) final
            # turn's tokens.
            entry["tokens_in"] += row.get("loop_tokens_in", row.get("tokens_in", 0))
            entry["tokens_out"] += row.get("loop_tokens_out", row.get("tokens_out", 0))

    lines = [f"eval run: {path.resolve().name}", ""]
    lines.append(f"{'CASE':<70} {'STATUS':<8} {'WALL(s)':>8} {'TOK IN':>10} {'TOK OUT':>8}  OUTCOMES")
    total_in = total_out = total_wall = 0.0
    counts: dict[str, int] = {}
    for test, e in sorted(tests.items()):
        counts[e["status"]] = counts.get(e["status"], 0) + 1
        total_in += e["tokens_in"]
        total_out += e["tokens_out"]
        total_wall += e["duration_s"]
        name = test.split("::", 1)[-1]
        lines.append(
            f"{name:<70} {e['status']:<8} {e['duration_s']:>8.1f} "
            f"{e['tokens_in']:>10} {e['tokens_out']:>8}  {','.join(e['outcomes']) or '-'}"
        )
    lines.append("")
    summary = ", ".join(f"{n} {status}" for status, n in sorted(counts.items()))
    lines.append(f"{len(tests)} case(s): {summary}")
    lines.append(f"totals: {int(total_in)} tokens in, {int(total_out)} tokens out, {total_wall:.0f}s wall")
    return "\n".join(lines)


def main(argv: list[str]) -> int:
    path = Path(argv[1]) if len(argv) > 1 else _RESULTS_DIR / "latest.jsonl"
    if not path.exists():
        print(f"no results at {path} -- run `make eval` first", file=sys.stderr)
        return 1
    print(summarize(path))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
