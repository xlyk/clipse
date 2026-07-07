"""Minimal LLM judge for evals needing a semantic yes/no (v2: ONE case, D3).

Built on deepagents_code.config.create_model -- the only model factory
reachable through a DIRECT dependency (`anthropic`/`langchain-anthropic` are
transitive-only in uv.lock, and importing a transitive is a bug in this repo).
The judge model is pinned to the smallest current anthropic model and does
NOT follow CLIPSE_EVAL_MODEL: verdicts must stay comparable across the model
matrix, so a codex run still judges with haiku (and skips D3 without
ANTHROPIC_API_KEY -- see harness.requires_judge).

Upgrade path (deferred): langsmith[pytest] judge-as-feedback once judge usage
grows past one case.
"""
from __future__ import annotations

import json
from typing import Any

from deepagents_code.config import create_model

JUDGE_MODEL = "anthropic:claude-haiku-4-5"

_PROMPT = """You are a strict evaluator. Judge the evidence against the rubric.

## Rubric
{rubric}

## Evidence
{evidence}

Answer with ONLY this JSON object, nothing else:
{{"pass": true or false, "reason": "<one short sentence>"}}"""


class JudgeError(RuntimeError):
    """The judge produced no parseable verdict, even after one retry."""


def _message_text(message: Any) -> str:
    """Extract the text of a LangChain AIMessage: str content, or the text
    blocks of a structured content list (same shape dac.py consumes)."""
    content = message.content
    if isinstance(content, str):
        return content
    return "".join(
        block.get("text", "")
        for block in content
        if isinstance(block, dict) and block.get("type") == "text"
    )


def _parse_verdict(text: str) -> bool | None:
    """Return the verdict, or None when the strict contract is absent."""
    start, end = text.find("{"), text.rfind("}")
    if start == -1 or end <= start:
        return None
    try:
        payload = json.loads(text[start : end + 1])
    except json.JSONDecodeError:
        return None
    verdict = payload.get("pass") if isinstance(payload, dict) else None
    return verdict if isinstance(verdict, bool) else None


def judge(rubric: str, evidence: str) -> bool:
    """One yes/no judgment. Raises JudgeError on an unparseable reply after
    one retry -- a broken judge must fail the case loudly, never pass it."""
    model = create_model(JUDGE_MODEL).model
    prompt = _PROMPT.format(rubric=rubric, evidence=evidence)
    messages = [{"role": "user", "content": prompt}]
    for _ in range(2):  # initial attempt + one retry
        verdict = _parse_verdict(_message_text(model.invoke(messages)))
        if verdict is not None:
            return verdict
        messages = [{"role": "user", "content": prompt + "\n\nReply with ONLY the JSON object."}]
    raise JudgeError('judge returned no parseable {"pass": bool} verdict after a retry')
