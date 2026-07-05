"""Tests for the structured status/title/handoff tail parser (`clipse_agent.tail`).

The coder lane ends its final message with an explicit STATUS/TITLE/HANDOFF
tail; `parse_structured_tail` reads it instead of scraping narration line 1.
The parser is tolerant by design: absent keys yield empty strings, the last
occurrence of a repeated key wins, and it never raises on malformed input.
"""

from __future__ import annotations

from clipse_agent.tail import StructuredTail, parse_structured_tail


def test_parse_done_status():
    tail = parse_structured_tail("STATUS: done")
    assert tail.status == "done"
    assert tail.blocked_reason == ""


def test_parse_blocked_status_captures_reason():
    tail = parse_structured_tail("STATUS: blocked: need REFLEX_CEREBRAS_API_KEY")
    assert tail.status == "blocked"
    assert tail.blocked_reason == "need REFLEX_CEREBRAS_API_KEY"


def test_missing_tail_yields_all_empty_fields():
    tail = parse_structured_tail("just some narration\nnothing structured here")
    assert tail == StructuredTail(status="", blocked_reason="", title="", handoff="")


def test_empty_text_yields_all_empty_fields():
    assert parse_structured_tail("") == StructuredTail()


def test_parse_title():
    tail = parse_structured_tail("STATUS: done\nTITLE: feat: add widget factory")
    assert tail.title == "feat: add widget factory"


def test_title_longer_than_72_chars_is_preserved_verbatim():
    # Truncation is the consumer's job (_commit_message caps at 72); the parser
    # returns the raw value untouched.
    long_title = "feat: " + "x" * 200
    tail = parse_structured_tail(f"STATUS: done\nTITLE: {long_title}")
    assert tail.title == long_title
    assert len(tail.title) > 72


def test_handoff_captures_multiple_lines_until_end():
    text = (
        "STATUS: done\n"
        "TITLE: feat: add widget\n"
        "HANDOFF:\n"
        "- chose drop semantics for the queue\n"
        "- added Widget.build(spec) -> Widget\n"
        "- did NOT wire the metrics counter\n"
    )
    tail = parse_structured_tail(text)
    assert tail.handoff == (
        "- chose drop semantics for the queue\n"
        "- added Widget.build(spec) -> Widget\n"
        "- did NOT wire the metrics counter"
    )


def test_key_line_after_handoff_terminates_its_capture():
    # HANDOFF captures until the next KEY line; a STATUS/TITLE after it is
    # parsed as its own key, never swallowed into the handoff body. (In the
    # documented tail HANDOFF is last, so this only guards misordered output.)
    text = "HANDOFF:\n- note one\nSTATUS: done"
    tail = parse_structured_tail(text)
    assert tail.status == "done"
    assert "STATUS" not in tail.handoff


def test_handoff_inline_value_on_key_line():
    tail = parse_structured_tail("HANDOFF: single inline note")
    assert tail.handoff == "single inline note"


def test_keys_are_case_insensitive():
    tail = parse_structured_tail("status: DONE\ntitle: fix: thing")
    assert tail.status == "done"
    assert tail.title == "fix: thing"


def test_last_occurrence_of_a_key_wins():
    tail = parse_structured_tail("STATUS: done\nTITLE: first\nTITLE: second")
    assert tail.title == "second"


def test_only_last_40_lines_are_scanned():
    # A STATUS buried above the last 40 lines is ignored -- the tail lives at
    # the very end of the message.
    text = "STATUS: done\n" + "\n".join(f"line {i}" for i in range(60))
    assert parse_structured_tail(text).status == ""


def test_parser_never_raises_on_arbitrary_text():
    # Belt-and-braces: any junk degrades to empty fields, never an exception.
    for junk in ("STATUS:", "TITLE:::", "HANDOFF", ":::", "STATUS: sideways"):
        parse_structured_tail(junk)
