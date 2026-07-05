"""Parse the STATUS/TITLE/HANDOFF tail a lane appends to its final message.

The coder lane is instructed (profiles/coder.py's system prompt) to end its
FINAL message with an explicit tail so the kernel reads a contract instead of
scraping narration line 1:

    STATUS: done            (or: STATUS: blocked: <what you need and why>)
    TITLE: <lowercase conventional-commit line, <=60 chars>
    HANDOFF:
    <3-8 bullet lines for the next agent>

`parse_structured_tail` is deliberately tolerant: a model that skips the
protocol degrades to legacy behavior (all-empty fields), never to an
exception. Consumers layer their own defaults on top (e.g. graphs/coder.py's
`_commit_message` falls back to the DAC summary headline when `title` is "").
"""

from __future__ import annotations

import re
from dataclasses import dataclass

_KEY_RE = re.compile(r"^(STATUS|TITLE|HANDOFF):\s*(.*)$", re.IGNORECASE)


@dataclass(frozen=True)
class StructuredTail:
    """The parsed STATUS/TITLE/HANDOFF tail of a lane's final message.

    Every field is a plain string that is "" when the corresponding key was
    absent, so a consumer can treat "" uniformly as "not provided".
    `blocked_reason` is the text after `blocked:` and is only meaningful when
    `status == "blocked"`.
    """

    status: str = ""
    blocked_reason: str = ""
    title: str = ""
    handoff: str = ""


def parse_structured_tail(text: str) -> StructuredTail:
    """Parse the STATUS/TITLE/HANDOFF tail from a lane's final message.

    Tolerant by design: absent keys yield empty strings, the last
    occurrence of a repeated key wins, and free text before the tail is
    ignored -- a model that skips the protocol degrades to legacy
    behavior, never to an exception. Only the last 40 lines are scanned,
    since the tail lives at the very end of the message. `HANDOFF:` captures
    every subsequent line until another `KEY:` line or the end of the text.
    """
    status = ""
    blocked_reason = ""
    title = ""
    handoff_lines: list[str] | None = None
    for line in text.splitlines()[-40:]:
        match = _KEY_RE.match(line.strip())
        if match:
            key, value = match.group(1).upper(), match.group(2).strip()
            if key == "STATUS":
                handoff_lines = None
                lowered = value.lower()
                if lowered.startswith("blocked"):
                    status = "blocked"
                    _, _, reason = value.partition(":")
                    blocked_reason = reason.strip()
                elif lowered.startswith("done"):
                    status = "done"
            elif key == "TITLE":
                handoff_lines = None
                title = value
            elif key == "HANDOFF":
                handoff_lines = [value] if value else []
        elif handoff_lines is not None:
            handoff_lines.append(line)
    handoff = "\n".join(handoff_lines).strip() if handoff_lines else ""
    return StructuredTail(status=status, blocked_reason=blocked_reason, title=title, handoff=handoff)
