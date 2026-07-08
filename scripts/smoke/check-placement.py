#!/usr/bin/env python3
"""R5: reviewer inline-comment placement validity against a REAL GitHub repo.

Report-only, optional -- requires `gh` auth and a completed smoke run. For
each smoke PR it compares posted inline review comments (gh api) against the
PR diff hunks, and also counts the findings the reviewer itself reported as
unplaceable in its summary comment ("N finding(s) could not be attached").

Usage:
  check-placement.py --repo owner/name [--pr N ...] [--self-test]

Exit code is always 0 unless gh itself fails -- placement rate is a metric,
not a gate.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys

HUNK_RE = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
UNATTACHED_RE = re.compile(r"(\d+) finding\(s\) could not be attached")
SMOKE_TITLE_RE = re.compile(r"^[A-Z]+-\d+[: ]")


def gh_json(args: list[str]):
    proc = subprocess.run(["gh", *args], capture_output=True, text=True)
    if proc.returncode != 0:
        raise SystemExit(f"gh {' '.join(args)} failed: {proc.stderr.strip()}")
    return json.loads(proc.stdout or "[]")


def hunk_lines(patch: str) -> set[int]:
    """RIGHT-side line numbers a patch's hunks cover (added + context lines)
    -- the lines GitHub accepts an inline review comment on."""
    covered: set[int] = set()
    current: int | None = None
    for raw in (patch or "").splitlines():
        match = HUNK_RE.match(raw)
        if match:
            current = int(match.group(1))
            continue
        if current is None or raw.startswith("-"):
            continue
        if raw.startswith(("+", " ")):
            covered.add(current)
            current += 1
    return covered


def check_pr(repo: str, number: int) -> tuple[int, int, int]:
    files = gh_json(["api", f"repos/{repo}/pulls/{number}/files", "--paginate"])
    hunks = {f["filename"]: hunk_lines(f.get("patch", "")) for f in files}
    comments = gh_json(["api", f"repos/{repo}/pulls/{number}/comments", "--paginate"])
    posted = len(comments)
    placed = sum(
        1 for c in comments
        if (c.get("line") or c.get("original_line")) in hunks.get(c.get("path", ""), set())
    )
    issue_comments = gh_json(["api", f"repos/{repo}/issues/{number}/comments", "--paginate"])
    unattached = sum(
        int(m.group(1))
        for c in issue_comments
        if (m := UNATTACHED_RE.search(c.get("body") or ""))
    )
    return posted, placed, unattached


def self_test() -> int:
    patch = "@@ -1,3 +1,4 @@\n context\n+added\n context\n-removed\n context\n@@ -10 +12,2 @@\n+a\n+b\n"
    lines = hunk_lines(patch)
    assert lines == {1, 2, 3, 4, 12, 13}, lines
    assert hunk_lines("") == set()
    assert UNATTACHED_RE.search("2 finding(s) could not be attached to a diff line").group(1) == "2"
    print("self-test ok")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo")
    parser.add_argument("--pr", type=int, action="append", default=[])
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.repo:
        parser.error("--repo is required (unless --self-test)")

    numbers = args.pr or [
        pr["number"]
        for pr in gh_json(["pr", "list", "--repo", args.repo, "--state", "all",
                           "--limit", "200", "--json", "number,title"])
        if SMOKE_TITLE_RE.match(pr["title"])
    ]
    total_posted = total_placed = total_unattached = 0
    print(f"{'PR':>5} {'POSTED':>7} {'PLACED':>7} {'UNATTACHED':>11}")
    for number in sorted(numbers):
        posted, placed, unattached = check_pr(args.repo, number)
        total_posted += posted
        total_placed += placed
        total_unattached += unattached
        print(f"{number:>5} {posted:>7} {placed:>7} {unattached:>11}")
    attempted = total_posted + total_unattached
    rate = (total_placed / attempted * 100) if attempted else 100.0
    print(f"\nplacement rate: {total_placed}/{attempted} attempted findings placed inline ({rate:.0f}%)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
