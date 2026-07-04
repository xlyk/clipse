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

SAFETY (non-negotiable, see design doc "Threat model"): `create_cli_agent`
must always be called with `auto_approve=False, interrupt_shell_only=True,
shell_allow_list=[...]`. Setting `auto_approve=True`
silently drops the shell allow-list — `restrictive_shell_allow_list` is
only populated `if interrupt_shell_only and not auto_approve`
(`agent.py:1336`), `ShellAllowListMiddleware` is only installed when that
list is non-nil (`agent.py:1597`), and `auto_approve` forces
`interrupt_on={}` (`agent.py:1612`). That combination must never appear in
this file.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Literal

from deepagents_code.agent import create_cli_agent
from deepagents_code.config import create_model
from deepagents_code.model_config import CODEX_PROVIDER
from langchain_core.messages import AIMessage
from langgraph.types import Command

if TYPE_CHECKING:
    from pathlib import Path

    from deepagents.backends import CompositeBackend
    from langgraph.checkpoint.base import BaseCheckpointSaver
    from langgraph.pregel import Pregel

    from clipse_agent.profiles.coder import CoderProfile

OutcomeHint = Literal["completed", "interrupted"]


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


def build_coder_agent(
    profile: CoderProfile,
    checkpointer: BaseCheckpointSaver | None,
    cwd: str | Path,
) -> tuple[Pregel[Any, Any, Any, Any], CompositeBackend]:
    """Build the Coder lane's DAC agent from its profile.

    Always calls `create_cli_agent` with the safe shell-enforcement
    combination documented in the module docstring. Never pass
    `auto_approve=True` here — it silently disables `shell_allow_list`.

    `enable_ask_user=True` is required, not incidental: `AskUserMiddleware`
    (`ask_user.py:256`) is the only source of a LangGraph `interrupt()` in
    this configuration (installing the shell middleware forces
    `interrupt_on={}`, `agent.py:1612`), so it is the sole way the agent can
    surface a `__interrupt__` — which the worker maps to
    `blocked(needs_input)`. With `enable_ask_user=False` that path is dead
    and an ambiguous issue can never block for input.

    Raises:
        DacError: wrapping anything `create_model` or `create_cli_agent` raises
            (including a `MissingCredentialsError` from either).
    """
    try:
        model: str | Any = profile.model
        if profile.model.split(":", 1)[0] == CODEX_PROVIDER:
            # init_chat_model can't resolve the DAC-only "openai_codex" provider
            # (raises ValueError); DAC's create_model wires the on-disk OAuth
            # token store and returns a ready BaseChatModel to hand over instead.
            model = create_model(profile.model).model
        return create_cli_agent(
            model,
            profile.assistant_id,
            system_prompt=profile.system_prompt,
            interactive=False,
            auto_approve=False,
            interrupt_shell_only=True,
            shell_allow_list=list(profile.shell_allow_list),
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


def _accumulate_message_chunk(
    data: tuple[Any, dict[str, Any]],
    text_parts: list[str],
) -> tuple[int, int]:
    """Fold one `messages`-mode chunk's usage/text into the running turn.

    Mutates `text_parts` in place with any text blocks found. Returns the
    `(input_tokens, output_tokens)` this chunk contributes; always `(0,
    0)` for anything that is not an `AIMessage` (e.g. a `ToolMessage`,
    which has no `usage_metadata` and whose `content_blocks` are tool
    output, not assistant text).
    """
    message_obj, _metadata = data
    if not isinstance(message_obj, AIMessage):
        return 0, 0

    usage = getattr(message_obj, "usage_metadata", None) or {}
    tokens_in = usage.get("input_tokens", 0) or 0
    tokens_out = usage.get("output_tokens", 0) or 0

    for block in getattr(message_obj, "content_blocks", None) or ():
        if isinstance(block, dict) and block.get("type") == "text":
            text = block.get("text", "")
            if text:
                text_parts.append(text)

    return tokens_in, tokens_out


async def drive_turn(
    agent_graph: Pregel[Any, Any, Any, Any],
    config: dict[str, Any],
    *,
    task_text: str | None = None,
    resume: Any | None = None,
    max_tokens: int | None,
) -> DacTurnResult:
    """Drive one turn of `agent_graph` to completion or interrupt.

    Exactly one of `task_text` (fresh turn) or `resume` (continuation
    after an interrupt, injected via `Command(resume=...)`) must be given.
    `config["configurable"]["thread_id"]` is what makes a resume pick up
    the prior checkpoint — DAC's own non-interactive runner cannot do
    this, so the kernel/worker must drive the graph directly, which is
    exactly what this function does.

    Accumulates `usage_metadata` across every `AIMessage` seen in the
    stream. If `max_tokens` is set and the running total exceeds it, the
    stream is abandoned immediately — no further chunks are pulled — and
    `token_ceiling_exceeded=True` is set on the result; the caller maps
    that to `outcome=blocked, block_kind=capability`.

    Raises:
        ValueError: if `task_text`/`resume` are both or neither given.
        DacError: wrapping any error raised while streaming the graph.
    """
    if (task_text is None) == (resume is None):
        raise ValueError(
            "drive_turn requires exactly one of task_text (fresh turn) or "
            "resume (continuation after an interrupt), not both or neither"
        )

    stream_input: dict[str, Any] | Command = (
        _fresh_turn_input(task_text) if task_text is not None else Command(resume=resume)
    )

    tokens_in = 0
    tokens_out = 0
    text_parts: list[str] = []
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
            elif mode == "messages":
                turn_in, turn_out = _accumulate_message_chunk(data, text_parts)
                tokens_in += turn_in
                tokens_out += turn_out

            if max_tokens is not None and (tokens_in + tokens_out) > max_tokens:
                token_ceiling_exceeded = True
                break
    except Exception as exc:
        thread_id = config.get("configurable", {}).get("thread_id")
        raise DacError(
            f"DAC turn failed while streaming the agent graph "
            f"(thread_id={thread_id!r}): {exc}"
        ) from exc

    outcome_hint: OutcomeHint = (
        "interrupted" if interrupt_payload is not None or token_ceiling_exceeded else "completed"
    )

    return DacTurnResult(
        outcome_hint=outcome_hint,
        final_text="".join(text_parts),
        tokens_in=tokens_in,
        tokens_out=tokens_out,
        interrupt_payload=interrupt_payload,
        token_ceiling_exceeded=token_ceiling_exceeded,
    )
