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

The Coder and Reviewer lanes are wired to real graphs (documentation is a
step inside the Coder graph, not a separate lane). The Git-operator lane is a
real `Lane` member the kernel deliberately never dispatches here: per decision
O/J amendment, it runs as deterministic Go
(`internal/gitops`), never a DAC worker -- so it, and any garbage string
that isn't a Lane member at all, are handled the same way: outcome=blocked,
block_kind=transient, no graph ever built or run.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys
import traceback
from collections.abc import Sequence
from typing import Any

from daytona import (
    DaytonaAuthenticationError,
    DaytonaAuthorizationError,
    DaytonaConnectionError,
    DaytonaError,
    DaytonaNotFoundError,
    DaytonaRateLimitError,
    DaytonaTimeoutError,
    DaytonaValidationError,
)
from langgraph.checkpoint.sqlite.aio import AsyncSqliteSaver
from pydantic import ValidationError

from clipse_agent.backends.contracts import BackendActionError, BackendActionRequest, BackendActionResult
from clipse_agent.backends.daytona import DaytonaLifecycle
from clipse_agent.backends.github import safe_error
from clipse_agent.contract import BlockKind, Lane, Outcome, Tokens, WorkerResult
from clipse_agent.graphs.coder import build_coder_graph
from clipse_agent.graphs.reviewer import build_reviewer_graph
from clipse_agent.profiles.coder import get_coder_docs_profile, get_coder_profile
from clipse_agent.profiles.reviewer import get_reviewer_profile
from clipse_agent.transcript import TranscriptWriter

_ZERO_TOKENS = Tokens(**{"in": 0, "out": 0})

# The env fallback for --max-tokens (see _resolve_max_tokens), named after
# the same CLIPSE_ prefix as CLIPSE_ISSUE_TEXT (graphs/coder.py) and
# CLIPSE_CONFIG (cli/dispatch.go).
_MAX_TOKENS_ENV_VAR = "CLIPSE_MAX_TOKENS"


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="clipse-worker")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--backend-action", default="")
    mode.add_argument("--lane", default="coder")
    parser.add_argument("--issue", default="")
    parser.add_argument("--run", default="")
    parser.add_argument("--thread", default="")
    parser.add_argument("--workspace", default="")
    parser.add_argument("--checkpoint-db", default="")
    parser.add_argument("--max-tokens", type=int, default=None)
    parser.add_argument("--model", default="")
    parser.add_argument("--docs-model", default="")
    parser.add_argument("--model-params", default="")
    parser.add_argument("--docs-model-params", default="")
    parser.add_argument("--shell-allow-list", default="")
    parser.add_argument("--docs-shell-allow-list", default="")
    parser.add_argument("--transcript", default="")
    parser.add_argument("--base-branch", default="")
    parser.add_argument("--backend-provider", default="daytona")
    parser.add_argument("--backend-role", default="")
    parser.add_argument("--repo-url", default="")
    parser.add_argument("--repo-slug", default="")
    parser.add_argument("--branch", default="")
    parser.add_argument("--sandbox-id", default="")
    parser.add_argument("--auto-stop-minutes", type=int, default=None)
    parser.add_argument("--reviewer-auto-delete-minutes", type=int, default=None)
    parser.add_argument("--snapshot", default="")
    parser.add_argument("--target", default="")
    return parser


def _backend_result_identity(args: argparse.Namespace) -> tuple[str, str]:
    """Return contract-valid identity values even for unsupported input."""

    action = args.backend_action if args.backend_action in {"ensure", "delete", "list"} else "ensure"
    provider = args.backend_provider if args.backend_provider == "daytona" else "daytona"
    return action, provider


def _backend_failure(
    args: argparse.Namespace,
    *,
    kind: str,
    operation: str,
    message: str,
) -> BackendActionResult:
    action, provider = _backend_result_identity(args)
    return BackendActionResult(
        action=action,
        provider=provider,
        ok=False,
        error_kind=kind,
        error_operation=operation,
        error=message,
    )


def _backend_request(args: argparse.Namespace) -> BackendActionRequest:
    return BackendActionRequest(
        action=args.backend_action,
        provider=args.backend_provider,
        repo_url=args.repo_url or None,
        repo_slug=args.repo_slug,
        base_branch=args.base_branch or None,
        branch=args.branch or None,
        issue_id=args.issue or None,
        run_id=args.run or None,
        role=args.backend_role or None,
        auto_stop_minutes=args.auto_stop_minutes,
        reviewer_auto_delete_minutes=args.reviewer_auto_delete_minutes,
        sandbox_id=args.sandbox_id or None,
        snapshot=args.snapshot or None,
        target=args.target or None,
    )


def _dispatch_backend_action(args: argparse.Namespace) -> BackendActionResult:
    """Run one lifecycle action and always return a parseable typed result."""

    if args.backend_provider != "daytona" or args.backend_action not in {"ensure", "delete", "list"}:
        return _backend_failure(
            args,
            kind="capability",
            operation="backend_action",
            message="unsupported backend provider or action",
        )
    if args.backend_role and args.backend_role not in {"coder", "reviewer"}:
        return _backend_failure(
            args,
            kind="capability",
            operation="backend_action",
            message="unsupported backend role",
        )

    try:
        request = _backend_request(args)
        lifecycle = DaytonaLifecycle()
        if request.action == "ensure":
            workspace = lifecycle.ensure(request)
            return BackendActionResult(
                action=request.action,
                provider=request.provider,
                ok=True,
                **workspace.model_dump(),
            )
        if request.action == "delete":
            workspace = lifecycle.delete(request)
            return BackendActionResult(
                action=request.action,
                provider=request.provider,
                ok=True,
                **workspace.model_dump(),
            )
        workspaces = lifecycle.list(request)
        return BackendActionResult(action=request.action, provider=request.provider, ok=True, workspaces=workspaces)
    except BackendActionError as exc:
        return _backend_failure(
            args,
            kind=exc.kind,
            operation=exc.operation,
            message=str(exc),
        )
    except (DaytonaAuthenticationError, DaytonaAuthorizationError):
        return _backend_failure(
            args,
            kind="needs_input",
            operation="daytona_auth",
            message="Daytona authentication is required",
        )
    except (DaytonaValidationError, DaytonaNotFoundError) as exc:
        return _backend_failure(
            args,
            kind="needs_input",
            operation="daytona_config",
            message=safe_error("daytona configuration", exc),
        )
    except (DaytonaTimeoutError, DaytonaConnectionError, DaytonaRateLimitError, TimeoutError) as exc:
        return _backend_failure(
            args,
            kind="transient",
            operation="daytona",
            message=safe_error("daytona", exc),
        )
    except ValidationError:
        return _backend_failure(
            args,
            kind="needs_input",
            operation="backend_action",
            message="invalid backend action request",
        )
    except (ImportError, AttributeError, TypeError) as exc:
        return _backend_failure(
            args,
            kind="capability",
            operation="daytona_sdk",
            message=safe_error("Daytona SDK", exc),
        )
    except DaytonaError as exc:
        return _backend_failure(
            args,
            kind="transient",
            operation="daytona",
            message=safe_error("daytona provider", exc),
        )
    except Exception as exc:  # noqa: BLE001 - unknown failures are not safe to retry automatically
        return _backend_failure(
            args,
            kind="capability",
            operation="daytona_sdk",
            message=safe_error("Daytona SDK", exc),
        )


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


def _parse_params(raw: str) -> dict[str, Any] | None:
    """Decode a `--model-params`/`--docs-model-params` flag value.

    The kernel hands these over as compact JSON only when the corresponding
    lane has a `model_params` block configured (`internal/spawn.LocalSpawner`
    omits the flag entirely otherwise, so `raw` defaults to `""` here) --
    mirroring `CoderProfile.model_params`/`ReviewerProfile.model_params`'s own
    "no config means None, not some empty-dict default" contract.
    """
    return json.loads(raw) if raw else None


def _parse_shell(raw: str) -> list[str] | None:
    """Decode a `--shell-allow-list`/`--docs-shell-allow-list` flag value.

    `None` means the `all` policy (unrestricted shell) — the kernel omits
    the flag entirely for an all-policy lane (`internal/spawn.workerArgs`),
    so `raw` defaults to `""` here, same as `_parse_params`'s "no config
    means None" contract. A present flag carries a JSON array of allowed
    command names, which becomes the profile's restrictive tuple.
    """
    return json.loads(raw) if raw else None


def _build_transcript(path: str) -> TranscriptWriter | None:
    """Build this run's transcript writer, or None when disabled.

    `""` (the default, and what `internal/spawn.workerArgs` sends whenever
    `WorkerSpec.TranscriptPath` is unset -- e.g. a hand-built test config
    with no `BoardDir`) means disabled: no writer is built, and the lane's
    graph gets `transcript=None`, its own default.
    """
    return TranscriptWriter(path) if path else None


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


async def _run_lane_graph(
    args: argparse.Namespace, build_graph: Any, *, lane: Lane, extra_kwargs: dict[str, Any] | None = None
) -> WorkerResult:
    """Drive one turn of `build_graph`'s graph and return its result.

    Shared by every lane `_dispatch` knows how to route to (coder/
    reviewer): every lane's graph is invoked with the exact same input-state
    shape (issue_id/run_id/thread_id/workspace/max_tokens/base_branch --
    issue_text is covered separately, by the dispatcher's own
    $CLIPSE_ISSUE_TEXT env injection that every lane's `load_context` already
    falls back to) and the exact same checkpointer wiring; only which
    graph-builder function to call differs, and `_dispatch` passes that in
    already resolved from this module's own globals (see its docstring on
    why that matters for monkeypatching). `base_branch` is meaningful only to
    the coder lane's own `sync_base` node today; the reviewer lane already
    reads it too (its PR-diff base), and it is simply unused input for any
    future lane that ignores it. The checkpointer is built from
    --checkpoint-db when the kernel gave one (design doc: one checkpointer
    database per issue, path owned by the kernel, shared by every lane that
    runs against that issue); otherwise the graph runs with no checkpointer
    at all -- still correct for a single turn, just without cross-process
    resume.

    `extra_kwargs` carries lane-specific keyword arguments `_dispatch`
    already resolved (e.g. the coder lane's `profile`/`docs_profile`) that
    get forwarded verbatim into `build_graph` alongside `checkpointer`. The
    `or {}` guard matters: `build_graph(**None)` raises `TypeError`, and a
    lane that has no extra kwargs (or a caller that omits the argument
    entirely) must still build cleanly.

    `lane` is `_dispatch`'s own already-matched Lane for this call, used
    ONLY to namespace the OUTER wrapping-graph's checkpoint thread_id below
    -- a fresh claim passes the identical raw `--thread` regardless of
    lane, so coder/reviewer runs against the same issue would
    otherwise collide on this wrapping graph's own checkpoint the moment
    they shared a physical checkpointer (design doc: "one checkpointer
    database per issue", not one per lane). This mirrors each lane graph's
    own inner-DAC-thread namespace one level down (graphs/coder.py's
    `_DAC_THREAD_NAMESPACE_SUFFIX` == "::dac", and the reviewer
    analogue) -- same problem, same fix, one layer higher. `input_state`'s
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
        "base_branch": args.base_branch,
    }
    config: dict[str, Any] = {"configurable": {"thread_id": f"{args.thread}::{lane.value}"}}

    if not args.checkpoint_db:
        graph = build_graph(checkpointer=None, **(extra_kwargs or {}))
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]

    async with AsyncSqliteSaver.from_conn_string(args.checkpoint_db) as checkpointer:
        graph = build_graph(checkpointer=checkpointer, **(extra_kwargs or {}))
        final_state = await graph.ainvoke(input_state, config)
        return final_state["result"]


async def _dispatch(args: argparse.Namespace) -> WorkerResult:
    """Route to the named lane's graph. "coder"/"reviewer" are implemented
    (documentation is a step inside the coder graph, not its own lane); any
    other Lane member (today: only "git_operator", which by design never gets
    a Python graph -- see the module docstring), or a string that isn't a Lane
    member at all, is reported as blocked/transient without building or running
    a graph.

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
        return await _run_lane_graph(
            args,
            build_coder_graph,
            lane=Lane.coder,
            extra_kwargs={
                "profile": get_coder_profile(
                    args.model or None,
                    model_params=_parse_params(args.model_params),
                    shell_allow_list=_parse_shell(args.shell_allow_list),
                ),
                "docs_profile": get_coder_docs_profile(
                    args.docs_model or None,
                    model_params=_parse_params(args.docs_model_params),
                    shell_allow_list=_parse_shell(args.docs_shell_allow_list),
                ),
                "transcript": _build_transcript(args.transcript),
            },
        )
    if lane == Lane.reviewer:
        return await _run_lane_graph(
            args,
            build_reviewer_graph,
            lane=Lane.reviewer,
            extra_kwargs={
                "profile": get_reviewer_profile(
                    args.model or None,
                    model_params=_parse_params(args.model_params),
                    shell_allow_list=_parse_shell(args.shell_allow_list),
                ),
                "transcript": _build_transcript(args.transcript),
            },
        )
    return _blocked_transient(
        args,
        lane=lane if lane is not None else Lane.coder,
        summary=f"clipse-worker: lane {args.lane!r} is not implemented",
    )


def main(argv: Sequence[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)

    if args.backend_action:
        result = _dispatch_backend_action(args)
        print(result.model_dump_json(exclude_none=True))
        return 0

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
