"""Tests for the Coder lane's documentation sub-step DAC profile.

`get_coder_docs_profile` describes the DAC turn the coder graph's `run_docs`
node drives right after coding and before the PR is opened. It reuses the
`CoderProfile` dataclass (a docs turn is a sub-step of the Coder lane, not a
separate lane) but carries a docs-only prompt + a restricted allow-list.
"""

import dataclasses

import pytest

from clipse_agent.profiles.coder import CoderProfile, get_coder_docs_profile, get_coder_profile


def test_get_coder_docs_profile_returns_a_coder_profile():
    assert isinstance(get_coder_docs_profile(), CoderProfile)


def test_coder_docs_profile_is_frozen_and_deterministic():
    profile = get_coder_docs_profile()
    assert dataclasses.is_dataclass(profile)
    assert get_coder_docs_profile() == get_coder_docs_profile()
    with pytest.raises(dataclasses.FrozenInstanceError):
        profile.model = "anthropic:some-other-model"


def test_assistant_id_is_distinct_from_the_coding_turn():
    docs = get_coder_docs_profile()
    assert docs.assistant_id == "clipse-coder-docs"
    # Must differ from the coding turn's identity so telemetry/checkpoints stay separable.
    assert docs.assistant_id != get_coder_profile().assistant_id


def test_model_is_a_provider_qualified_placeholder_not_a_key():
    profile = get_coder_docs_profile()
    assert isinstance(profile.model, str) and profile.model
    assert ":" in profile.model
    provider, _, name = profile.model.partition(":")
    assert provider and name
    assert not profile.model.lower().startswith("sk-")
    assert "key" not in profile.model.lower()


def test_get_coder_docs_profile_model_override():
    assert get_coder_docs_profile("openai_codex:gpt-5.5").model == "openai_codex:gpt-5.5"


def test_get_coder_docs_profile_default_preserved():
    assert get_coder_docs_profile().model == "anthropic:claude-sonnet-4-6"


def test_get_coder_docs_profile_model_params_override():
    profile = get_coder_docs_profile(model_params={"reasoning_effort": "high"})

    assert profile.model_params == {"reasoning_effort": "high"}


def test_get_coder_docs_profile_model_params_default_is_none():
    assert get_coder_docs_profile().model_params is None


def test_prompt_targets_the_uncommitted_pre_review_change_not_a_merge():
    prompt = get_coder_docs_profile().system_prompt
    assert prompt.strip()
    lowered = prompt.lower()
    assert "worktree" in lowered
    assert "uncommitted" in lowered
    assert "git diff" in lowered  # inspect the not-yet-committed change
    # Docs ride the coder's own commit/PR, so the prompt must NOT describe a
    # post-merge context (the whole point of retiring the scribe lane).
    assert "merged" not in lowered
    assert "same commit" in lowered and "same pull request" in lowered


def test_prompt_is_docs_only_and_allows_a_no_op():
    lowered = get_coder_docs_profile().system_prompt.lower()
    assert "only write documentation" in lowered
    assert "never edit application or test source code" in lowered
    assert "no-op" in lowered


def test_shell_allow_list_is_docs_scoped_and_excludes_the_source_toolchain():
    profile = get_coder_docs_profile()
    expected = {"git", "gh", "ls", "cat", "grep", "rg", "find", "mkdir"}
    assert set(profile.shell_allow_list) == expected
    # None of the coder's source-toolchain commands: the docs turn only touches docs.
    for forbidden in ("sed", "go", "uv", "python", "python3", "make"):
        assert forbidden not in profile.shell_allow_list
    assert len(profile.shell_allow_list) == len(set(profile.shell_allow_list))
    assert all(isinstance(cmd, str) and cmd for cmd in profile.shell_allow_list)


def test_shell_allow_list_is_immutable():
    with pytest.raises((AttributeError, TypeError)):
        get_coder_docs_profile().shell_allow_list.append("rm")
