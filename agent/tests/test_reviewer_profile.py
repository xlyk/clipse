"""Tests for the Reviewer lane's DAC profile.

The profile is a frozen, plain-data description of how the Reviewer lane's
DAC agent should be built (`deepagents_code.agent.create_cli_agent`); it
carries no live model client, no secrets, and no I/O -- mirrors
`test_profiles.py`'s coverage of `CoderProfile`, adjusted for the Reviewer
lane's read-mostly allow-list.
"""

import dataclasses

import pytest

from clipse_agent.profiles.reviewer import ReviewerProfile, get_reviewer_profile


def test_get_reviewer_profile_returns_a_reviewer_profile():
    profile = get_reviewer_profile()

    assert isinstance(profile, ReviewerProfile)


def test_reviewer_profile_is_frozen():
    profile = get_reviewer_profile()

    assert dataclasses.is_dataclass(profile)
    with pytest.raises(dataclasses.FrozenInstanceError):
        profile.model = "anthropic:some-other-model"


def test_get_reviewer_profile_is_deterministic():
    assert get_reviewer_profile() == get_reviewer_profile()


def test_assistant_id_is_clipse_reviewer():
    profile = get_reviewer_profile()

    assert profile.assistant_id == "clipse-reviewer"


def test_model_is_a_provider_qualified_placeholder_not_a_key():
    profile = get_reviewer_profile()

    assert isinstance(profile.model, str)
    assert profile.model
    assert ":" in profile.model
    provider, _, name = profile.model.partition(":")
    assert provider and name
    assert not profile.model.lower().startswith("sk-")
    assert "key" not in profile.model.lower()


def test_get_reviewer_profile_model_override():
    assert get_reviewer_profile("openai_codex:gpt-5.5").model == "openai_codex:gpt-5.5"


def test_get_reviewer_profile_default_preserved():
    assert get_reviewer_profile().model == "anthropic:claude-opus-4-6"


def test_reviewer_model_is_distinct_from_coder_model():
    # Design doc: "Optionally run the Reviewer lane on a stronger or
    # distinct model to reduce correlated blind spots" -- Coder and
    # Reviewer sharing one model family means a reviewer approving its own
    # sibling's code is advisory signal, not a safety guarantee.
    from clipse_agent.profiles.coder import get_coder_profile

    assert get_reviewer_profile().model != get_coder_profile().model


def test_system_prompt_covers_diff_review_and_verdict_protocol():
    profile = get_reviewer_profile()

    assert isinstance(profile.system_prompt, str)
    prompt = profile.system_prompt.lower()
    assert profile.system_prompt.strip()
    assert "diff" in prompt
    assert "verdict" in prompt
    assert "pass" in prompt
    assert "changes_requested" in prompt
    assert "inline" in prompt
    # Advisory-only, never a sufficient merge gate on its own.
    assert "advisory" in prompt or "never" in prompt


def test_system_prompt_forbids_editing():
    profile = get_reviewer_profile()
    prompt = profile.system_prompt.lower()

    assert "read-mostly" in prompt or "read mostly" in prompt
    assert "commit" in prompt  # explicitly told this is NOT its job


def test_shell_allow_list_is_read_mostly_no_destructive_commands():
    profile = get_reviewer_profile()

    expected = {"git", "gh", "cat", "ls", "grep", "rg", "find"}
    assert set(profile.shell_allow_list) == expected
    assert len(profile.shell_allow_list) == len(set(profile.shell_allow_list))
    assert all(isinstance(cmd, str) and cmd for cmd in profile.shell_allow_list)

    # No write/execute-capable commands the Coder lane's own list has.
    for destructive in ("sed", "mkdir", "go", "uv", "python", "python3", "make", "echo"):
        assert destructive not in profile.shell_allow_list


def test_shell_allow_list_is_immutable():
    profile = get_reviewer_profile()

    with pytest.raises((AttributeError, TypeError)):
        profile.shell_allow_list.append("rm")
