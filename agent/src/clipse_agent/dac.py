"""DAC engine wrapper: build the Coder lane's agent and drive one turn.

`deepagents_code.agent.create_cli_agent` returns an already-compiled
LangGraph graph (a `Pregel` instance) that the caller drives in-process via
`.astream`; it is not a CLI process to shell out to. Per the DAC API spike
findings (docs/design/2026-07-01-clipse-design.md, "DAC API spike
findings", verified against `deepagents_code` 0.1.22 source):

- The bundled headless runner (`non_interactive.run_non_interactive`) has
  no `thread_id`/resume parameter and always starts a fresh thread, so it
  cannot serve continuation turns. Non-interactive resume-by-thread-id
  only works by driving the returned graph object directly, which is what
  this module does.
- DAC exposes no `stop_reason`/`finish_reason`. A turn is "interrupted"
  only when an `updates`-mode chunk carries a non-empty `__interrupt__`
  list (`non_interactive._process_interrupts` treats an empty list as no
  interrupt); otherwise the turn ran to completion.
- Token usage lives on each `AIMessage.usage_metadata` in `messages`-mode
  chunks (`non_interactive._process_ai_message`); DAC keeps no aggregate
  itself, so this module accumulates it turn-by-turn.

SAFETY (see design doc "Threat model" and the per-lane shell-policy decision,
2026-07-07, AGENTS.md): `create_cli_agent` is called in exactly one of two
sanctioned modes, chosen solely by `profile.shell_allow_list`:

- Restrictive (`shell_allow_list` is a tuple): `auto_approve=False,
  interrupt_shell_only=True, shell_allow_list=[...]`. `auto_approve=True`
  would silently drop the shell allow-list — `restrictive_shell_allow_list`
  is only populated `if interrupt_shell_only and not auto_approve`
  (`agent.py:1336`), `ShellAllowListMiddleware` is only installed when that
  list is non-nil (`agent.py:1597`), and `auto_approve` forces
  `interrupt_on={}` (`agent.py:1612`) — so that combination is impossible by
  construction here: this file never sets `auto_approve=True` while also
  passing a list.
- Unrestricted (`shell_allow_list` is `None`, the default from
  `profiles.coder.get_coder_profile`/`get_coder_docs_profile` and
  `profiles.reviewer.get_reviewer_profile`): `auto_approve=True,
  interrupt_shell_only=False, shell_allow_list=None`. No
  `ShellAllowListMiddleware` is installed and DAC's own dangerous-pattern
  checks (redirects, env-prefix commands, heredocs) never run; a
  prompt-injected issue body can run arbitrary shell in the worker's own
  process. Accepted for this project's single-tenant, personal-use posture
  — see AGENTS.md's threat-model note; the C11 injection eval is the
  standing monitor.

Both modes set `interrupt_on={}` inside DAC (`agent.py:1612` — either because
`auto_approve` is `True`, or because the shell middleware installed under
`interrupt_shell_only` takes over shell approval instead) and both pass
`enable_ask_user=True`, which keeps the `blocked(needs_input)` interrupt path
alive regardless of mode (`AskUserMiddleware`, `ask_user.py:256`, is the only
source of a LangGraph `interrupt()` once the shell middleware or
`auto_approve` forces `interrupt_on={}`). Verified against
`deepagents_code.agent` 0.1.22, lines ~1336/1612.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Literal

from deepagents_code.agent import create_cli_agent
from deepagents_code.config import create_model
from deepagents_code.model_config import CODEX_PROVIDER
from langchain_core.messages import AIMessage, AIMessageChunk, ToolMessage
from langgraph.types import Command

if TYPE_CHECKING:
    from pathlib import Path

    from deepagents.backends import CompositeBackend
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.pregel import Pregel

    from clipse_agent.profiles.coder import CoderProfile

OutcomeHint = Literal["completed", "interrupted"]

# One event dict in, nothing out. The real caller is
# `clipse_agent.transcript.TranscriptWriter.bind(...)`'s returned sink, which
# already merges lane/run_id/thread_id/assistant_id/model context into every
# event and never raises -- drive_turn does not wrap event_sink calls itself.
EventSink = Callable[[dict[str, Any]], None]

# Shared cap for the two transcript fields that can balloon per-message: an
# `assistant` event's accumulated text and a `tool_result` event's content
# (2026-07-07 controller amendment: one constant, not two). The other
# free-form fields -- `turn_start.task_text` and `interrupt.payload` -- ride
# uncapped deliberately: each appears at most once per turn and is normally
# small (an issue body, an ask-user prompt). Generous enough to keep a shell
# command's real output/error or a long narration legible in the transcript,
# small enough that one runaway value can't balloon the per-issue file.
_TRANSCRIPT_TEXT_LIMIT = 8_000


def _truncate_for_transcript(text: str, limit: int = _TRANSCRIPT_TEXT_LIMIT) -> str:
    if len(text) <= limit:
        return text
    return text[:limit] + f"...<truncated at {limit} chars>"


class DacError(RuntimeError):
    """Raised when building or driving a DAC agent fails.

    Always raised with `from exc` so the original `deepagents_code`/
    LangGraph traceback survives in `__cause__`.
    """


@dataclass(frozen=True)
class DacTurnResult:
    """Outcome of a single `drive_turn` call.

    `outcome_hint` is DAC-native signal only ("did the stream end clean or
    pause"), not the worker's own `Outcome` enum (done/blocked/...) —
    mapping DAC's outcome to the contract's `Outcome` is the caller's job,
    using `outcome_hint`, `interrupt_payload`, and `token_ceiling_exceeded`
    together (e.g. `token_ceiling_exceeded` maps to
    `blocked`/`capability` regardless of `outcome_hint`).
    """

    outcome_hint: OutcomeHint
    final_text: str
    tokens_in: int
    tokens_out: int
    interrupt_payload: list[Any] | None = None
    token_ceiling_exceeded: bool = False
    # last_text is the text blocks of only the FINAL AIMessage (by message id)
    # in the stream, whereas final_text concatenates every AIMessage's text
    # across the whole turn. A consumer that parses a structured tail
    # (STATUS/TITLE/HANDOFF) reads last_text so it never scrapes earlier
    # narration; final_text stays the full-turn audit trail (checkpoint/PR
    # body). Defaults to "" so callers that never set it are unaffected.
    last_text: str = ""


def build_coder_agent(
    profile: CoderProfile,
    checkpointer: BaseCheckpointSaver | None,
    cwd: str | Path,
) -> tuple[Pregel[Any, Any, Any, Any], CompositeBackend]:
    """Build the Coder lane's DAC agent from its profile.

    Routes to exactly one of the two sanctioned modes documented in the
    module docstring, chosen solely by `profile.shell_allow_list`:
    `None` (the default) builds an unrestricted agent (`auto_approve=True`,
    no allow-list); a tuple builds the original restrictive agent
    (`auto_approve=False, interrupt_shell_only=True, shell_allow_list=[...]`).
    The forbidden combination — `auto_approve=True` alongside a configured
    list — is impossible by construction: this function never passes both.

    `enable_ask_user=True` is required, not incidental, in BOTH modes:
    `AskUserMiddleware` (`ask_user.py:256`) is the only source of a
    LangGraph `interrupt()` once `interrupt_on={}` is forced -- by
    `auto_approve` in the unrestricted mode, or by installing the shell
    middleware in the restrictive mode (`agent.py:1612`) -- so it is the
    sole way the agent can surface a `__interrupt__`, which the worker maps
    to `blocked(needs_input)`. With `enable_ask_user=False` that path is
    dead and an ambiguous issue can never block for input.

    When `profile.context_window_tokens` is set (the default), the model is
    always resolved via `create_model` -- even for a plain-string provider
    with no `model_params` -- and its `.profile["max_input_tokens"]` is set
    to that value before `create_cli_agent` sees it. This lowers the trigger
    DAC's already-installed auto-summarizer (`SummarizationMiddleware`) uses,
    which is a fraction (0.85) of that same profile value per round (see the
    auto-compaction spike, docs/design's "DAC API spike findings", Option
    3). This is the SAFETY-neutral lever: only the model's advertised
    profile changes, never the `create_cli_agent` SAFETY args below.

    Since `context_window_tokens` defaults to non-`None`, this routes
    *every* lane through `create_model` now, not just `openai_codex`/
    `model_params` ones -- so every lane also picks up what `create_model`
    does unconditionally: applying any `~/.deepagents/config.toml`
    provider-profile overrides and an early fail-fast credential check.
    Dormant in the worker's scrubbed env today (no config.toml overrides in
    play, and credentials are already required either way), but a
    deliberate widening of `create_model`'s reach worth knowing about.

    Raises:
        DacError: wrapping anything `create_model` or `create_cli_agent` raises
            (including a `MissingCredentialsError` from either).
    """
    try:
        provider = profile.model.split(":", 1)[0]
        # create_cli_agent takes a bare model spec string; create_model is the
        # only one of the two that accepts extra_kwargs, and the only one
        # that returns a model *object* (vs. a bare spec string) whose
        # `.profile` can be mutated. So any lane carrying model_params or
        # context_window_tokens needs create_model even off the codex
        # provider -- not just openai_codex, whose plain string
        # init_chat_model can't resolve (raises ValueError) and which
        # create_model's on-disk OAuth token store handles regardless of
        # model_params.
        use_create_model = (
            provider == CODEX_PROVIDER
            or bool(profile.model_params)
            or profile.context_window_tokens is not None
        )
        model: str | Any = profile.model
        if use_create_model:
            model = create_model(profile.model, extra_kwargs=profile.model_params or None).model
        if profile.context_window_tokens is not None:
            # Lower DAC's already-installed SummarizationMiddleware trigger
            # (0.85 x model.profile["max_input_tokens"] per round --
            # compute_summarization_defaults) well below a big-context-window
            # model's real limit, so a long turn compacts itself instead of
            # ballooning. Any other profile keys the model already carries
            # survive the merge untouched.
            model.profile = {
                **(getattr(model, "profile", None) or {}),
                "max_input_tokens": profile.context_window_tokens,
            }
        # Two-mode routing (module docstring SAFETY block): shell_allow_list
        # is None (unrestricted, the default) or a tuple (restrictive) --
        # never anything else, so this is the only branch point.
        unrestricted = profile.shell_allow_list is None
        return create_cli_agent(
            model,
            profile.assistant_id,
            system_prompt=profile.system_prompt,
            interactive=False,
            auto_approve=unrestricted,
            interrupt_shell_only=not unrestricted,
            shell_allow_list=(list(profile.shell_allow_list) if not unrestricted else None),
            enable_ask_user=True,
            enable_shell=True,
            checkpointer=checkpointer,
            cwd=cwd,
        )
    except Exception as exc:
        raise DacError(
            f"failed to build DAC agent for assistant_id={profile.assistant_id!r}: {exc}"
        ) from exc


def _fresh_turn_input(task_text: str) -> dict[str, Any]:
    return {"messages": [{"role": "user", "content": task_text}]}


def _extract_interrupt_payload(data: dict[str, Any]) -> list[Any] | None:
    """Return the interrupt payload for an `updates` chunk, or None.

    Mirrors `deepagents_code.non_interactive._process_interrupts`: the
    `__interrupt__` key can be present with an empty list, which is not a
    real interrupt.
    """
    interrupts = data.get("__interrupt__") or []
    if not interrupts:
        return None
    return [getattr(item, "value", item) for item in interrupts]


def _flush_pending(event_sink: EventSink | None, pending: dict[str, Any]) -> None:
    """Emit the just-finished logical AIMessage's accumulated text and tool
    calls as transcript events (`assistant`, then one `tool_call` per call).

    A logical message is "finished" once a chunk for a DIFFERENT message id
    arrives (see `_accumulate_message_chunk`) or the stream ends -- `drive_turn`
    calls this once more after its loop to flush whatever message was still
    pending when the loop exited (including a token-ceiling abort mid-message).
    A no-op when `event_sink` is None (transcripts disabled) or `pending` has
    never seen a chunk yet (the very first call, before any message arrived).

    The `assistant` event's text is truncated to `_TRANSCRIPT_TEXT_LIMIT`
    (2026-07-07 controller amendment: the same cap `tool_result` content
    gets) -- but only in the emitted event copy. `pending["parts"]` itself is
    never mutated, since it also feeds `text_parts`/`DacTurnResult.last_text`,
    which `parse_structured_tail` reads for STATUS/TITLE/HANDOFF -- those must
    stay untruncated regardless of whether a transcript is attached.
    """
    if event_sink is None or "parts" not in pending:
        return
    text = "".join(pending.get("parts") or [])
    if text:
        event_sink({"event": "assistant", "text": _truncate_for_transcript(text)})
    for call in pending.get("tool_calls") or []:
        event_sink({"event": "tool_call", "name": call.get("name"), "args": call.get("args")})


def _accumulate_message_chunk(
    data: tuple[Any, dict[str, Any]],
    text_parts: list[str],
    last_message: dict[str, Any],
    event_sink: EventSink | None = None,
) -> tuple[int, int]:
    """Fold one `messages`-mode chunk's usage/text/tool-calls into the
    running turn, and (when `event_sink` is set) emit transcript events.

    Mutates `text_parts` in place with any text blocks found (the whole
    turn's text), and `last_message` (its `"id"`/`"parts"`/`"tool_calls"`/
    `"chunk_sum"` keys) so that only the FINAL AIMessage's data survives:
    streaming delivers one logical message as several chunks sharing an id,
    so `parts` accumulates text across those, and `chunk_sum` accumulates the
    raw chunks themselves via `AIMessageChunk.__add__` -- LangChain's own
    documented way to reassemble a streamed tool call's fragmented `args`
    JSON (a single chunk's own `.tool_calls` is often incomplete mid-stream;
    only the merged sum is reliably complete). All four keys reset whenever a
    chunk carrying a new message id arrives. `tool_calls` always holds the
    best snapshot for the CURRENT message: `chunk_sum.tool_calls` for a
    genuine `AIMessageChunk`, or the message's own `.tool_calls` for a
    complete, non-chunk `AIMessage` (some `messages`-mode emissions -- e.g. a
    subgraph's finished output folded into state -- are already complete,
    not chunks; merging those with `+` would raise).

    On a message-id transition, the JUST-FINISHED message's accumulated text
    and tool calls are flushed as transcript events (`_flush_pending`) BEFORE
    the reset; `drive_turn` calls `_flush_pending` once more after the stream
    ends, for whichever message was still pending when the loop exited.

    Returns the `(input_tokens, output_tokens)` this chunk contributes;
    always `(0, 0)` for anything that is not an `AIMessage` (e.g. a
    `ToolMessage`, which has no `usage_metadata` and whose `content_blocks`
    are tool output, not assistant text -- and which never touches
    `last_message`, so an interleaved tool result can't reset the final
    message's accumulated text). When `event_sink` is set, a `ToolMessage`
    instead emits its own `tool_result` event directly, with no accumulation
    needed: a tool's result arrives as one complete message, never streamed
    in fragments.
    """
    message_obj, _metadata = data
    if not isinstance(message_obj, AIMessage):
        if event_sink is not None and isinstance(message_obj, ToolMessage):
            event_sink(
                {
                    "event": "tool_result",
                    "name": message_obj.name,
                    "status": message_obj.status,
                    "content": _truncate_for_transcript(str(message_obj.content)),
                }
            )
        return 0, 0

    usage = getattr(message_obj, "usage_metadata", None) or {}
    tokens_in = usage.get("input_tokens", 0) or 0
    tokens_out = usage.get("output_tokens", 0) or 0

    message_id = getattr(message_obj, "id", None)
    if "parts" not in last_message or message_id != last_message.get("id"):
        _flush_pending(event_sink, last_message)
        last_message.clear()
        last_message["id"] = message_id
        last_message["parts"] = []

    for block in getattr(message_obj, "content_blocks", None) or ():
        if isinstance(block, dict) and block.get("type") == "text":
            text = block.get("text", "")
            if text:
                text_parts.append(text)
                last_message["parts"].append(text)

    if isinstance(message_obj, AIMessageChunk):
        prior_sum = last_message.get("chunk_sum")
        merged = message_obj if prior_sum is None else prior_sum + message_obj
        last_message["chunk_sum"] = merged
        last_message["tool_calls"] = merged.tool_calls
    elif message_obj.tool_calls:
        last_message["tool_calls"] = message_obj.tool_calls

    return tokens_in, tokens_out


async def drive_turn(
    agent_graph: Pregel[Any, Any, Any, Any],
    config: dict[str, Any],
    *,
    task_text: str | None = None,
    resume: Any | None = None,
    max_tokens: int | None,
    event_sink: EventSink | None = None,
) -> DacTurnResult:
    """Drive one turn of `agent_graph` to completion or interrupt.

    Exactly one of `task_text` (fresh turn) or `resume` (continuation
    after an interrupt, injected via `Command(resume=...)`) must be given.
    `config["configurable"]["thread_id"]` is what makes a resume pick up
    the prior checkpoint — DAC's own non-interactive runner cannot do
    this, so the kernel/worker must drive the graph directly, which is
    exactly what this function does.

    Accumulates `usage_metadata` across every `AIMessage` seen in the
    stream for reporting (`tokens_in`/`tokens_out` on the result are
    cumulative across the whole turn). The ceiling check is per-round,
    not cumulative: `max_tokens` caps the largest single round's input
    (context) tokens, a post-compaction runaway guard — DAC's built-in
    auto-summarizer already bounds each round's context, so a healthy
    turn can run arbitrarily long without its cumulative spend ever
    being compared against `max_tokens`. If any one round's input
    tokens exceed `max_tokens`, the stream is abandoned immediately —
    no further chunks are pulled — and `token_ceiling_exceeded=True` is
    set on the result; the caller maps that to `outcome=blocked,
    block_kind=capability`.

    `event_sink`, when given, receives one dict per transcript event as the
    turn is driven: `turn_start` (with `task_text`, `None` on a `resume`
    call) before the stream starts; `assistant`/`tool_call` once per logical
    message (flushed on a message-id transition or at stream end -- see
    `_flush_pending`); `tool_result` per `ToolMessage`; `interrupt` when one
    is detected; and `turn_end` (`outcome_hint`, `tokens_in`, `tokens_out`)
    once the stream ends normally. A turn that dies mid-stream first flushes
    the still-pending partial message (its `assistant`/`tool_call` events),
    then emits a best-effort `turn_end` carrying only `error` (`str(exc)`)
    before the `DacError` is raised -- `event_sink` is assumed not to raise
    (its real caller, `TranscriptWriter.bind`'s returned sink, already
    swallows everything), so this cannot mask the original exception;
    `drive_turn` does not wrap any other sink call in its own try/except.

    Raises:
        ValueError: if `task_text`/`resume` are both or neither given.
        DacError: wrapping any error raised while streaming the graph.
    """
    if (task_text is None) == (resume is None):
        raise ValueError(
            "drive_turn requires exactly one of task_text (fresh turn) or "
            "resume (continuation after an interrupt), not both or neither"
        )

    if event_sink is not None:
        event_sink({"event": "turn_start", "task_text": task_text})

    stream_input: dict[str, Any] | Command = (
        _fresh_turn_input(task_text) if task_text is not None else Command(resume=resume)
    )

    tokens_in = 0
    tokens_out = 0
    text_parts: list[str] = []
    last_message: dict[str, Any] = {}
    interrupt_payload: list[Any] | None = None
    token_ceiling_exceeded = False

    try:
        async for _namespace, mode, data in agent_graph.astream(
            stream_input,
            stream_mode=["messages", "updates"],
            subgraphs=True,
            config=config,
        ):
            if mode == "updates" and isinstance(data, dict) and "__interrupt__" in data:
                payload = _extract_interrupt_payload(data)
                if payload is not None:
                    interrupt_payload = payload
                    if event_sink is not None:
                        event_sink({"event": "interrupt", "payload": repr(payload)})
            elif mode == "messages":
                turn_in, turn_out = _accumulate_message_chunk(data, text_parts, last_message, event_sink)
                tokens_in += turn_in
                tokens_out += turn_out

                if max_tokens is not None and turn_in > max_tokens:
                    token_ceiling_exceeded = True
                    break
    except Exception as exc:
        # Controller amendment (2026-07-07): a crashed turn must still leave
        # a trace -- postmortems are the feature's whole point. Flush the
        # still-pending partial message first, so assistant text/tool calls
        # streamed right before the crash land in the transcript (prime
        # postmortem material), then mark the turn dead. event_sink is
        # assumed not to raise (see docstring), so this cannot mask exc.
        _flush_pending(event_sink, last_message)
        if event_sink is not None:
            event_sink({"event": "turn_end", "error": str(exc)})
        thread_id = config.get("configurable", {}).get("thread_id")
        raise DacError(
            f"DAC turn failed while streaming the agent graph "
            f"(thread_id={thread_id!r}): {exc}"
        ) from exc

    _flush_pending(event_sink, last_message)

    outcome_hint: OutcomeHint = (
        "interrupted" if interrupt_payload is not None or token_ceiling_exceeded else "completed"
    )

    if event_sink is not None:
        event_sink(
            {
                "event": "turn_end",
                "outcome_hint": outcome_hint,
                "tokens_in": tokens_in,
                "tokens_out": tokens_out,
            }
        )

    return DacTurnResult(
        outcome_hint=outcome_hint,
        final_text="".join(text_parts),
        tokens_in=tokens_in,
        tokens_out=tokens_out,
        interrupt_payload=interrupt_payload,
        token_ceiling_exceeded=token_ceiling_exceeded,
        last_text="".join(last_message.get("parts", [])),
    )
