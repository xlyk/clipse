"""Tests for the Scribe lane's DAC profile.

The profile is a frozen, plain-data description of how the Scribe lane's DAC
agent should be built (`deepagents_code.agent.create_cli_agent`); it carries
no live model client, no secrets, and no I/O -- mirrors `test_profiles.py`'s
and `test_reviewer_profile.py`'s coverage, adjusted for the Scribe lane's
docs-writing (not read-mostly, not source-editing) allow-list.
"""

import dataclasses

import pytest

from clipse_agent.profiles.scribe import ScribeProfile, get_scribe_profile


def test_get_scribe_profile_returns_a_scribe_profile():
    profile = get_scribe_profile()

    assert isinstance(profile, ScribeProfile)


def test_scribe_profile_is_frozen():
    profile = get_scribe_profile()

    assert dataclasses.is_dataclass(profile)
    with pytest.raises(dataclasses.FrozenInstanceError):
        profile.model = "anthropic:some-other-model"


def test_get_scribe_profile_is_deterministic():
    assert get_scribe_profile() == get_scribe_profile()


def test_assistant_id_is_clipse_scribe():
    profile = get_scribe_profile()

    assert profile.assistant_id == "clipse-scribe"


def test_model_is_a_provider_qualified_placeholder_not_a_key():
    profile = get_scribe_profile()

    assert isinstance(profile.model, str)
    assert profile.model
    assert ":" in profile.model
    provider, _, name = profile.model.partition(":")
    assert provider and name
    assert not profile.model.lower().startswith("sk-")
    assert "key" not in profile.model.lower()


def test_system_prompt_covers_worktree_docs_merge_and_noop():
    profile = get_scribe_profile()

    assert isinstance(profile.system_prompt, str)
    prompt = profile.system_prompt.lower()
    assert profile.system_prompt.strip()
    assert "worktree" in prompt
    assert "doc" in prompt
    assert "merged" in prompt
    assert "no-op" in prompt
    assert "pull request" in prompt
    # Must stop on either terminal condition, not loop forever.
    assert "done" in prompt
    assert "blocked" in prompt


def test_system_prompt_forbids_source_edits():
    profile = get_scribe_profile()
    prompt = profile.system_prompt.lower()

    assert "only write documentation" in prompt or "never edit" in prompt


def test_shell_allow_list_is_minimal_but_sufficient():
    profile = get_scribe_profile()

    expected = {"git", "gh", "ls", "cat", "grep", "rg", "find", "mkdir"}
    assert set(profile.shell_allow_list) == expected
    # No duplicates and no blank entries snuck in.
    assert len(profile.shell_allow_list) == len(set(profile.shell_allow_list))
    assert all(isinstance(cmd, str) and cmd for cmd in profile.shell_allow_list)

    # No source-toolchain commands -- the Coder lane's own list has these;
    # the Scribe lane only ever touches docs.
    for toolchain_cmd in ("sed", "go", "uv", "python", "python3", "make"):
        assert toolchain_cmd not in profile.shell_allow_list


def test_shell_allow_list_is_immutable():
    profile = get_scribe_profile()

    with pytest.raises((AttributeError, TypeError)):
        profile.shell_allow_list.append("rm")
