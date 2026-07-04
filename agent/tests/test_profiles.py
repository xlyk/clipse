"""Tests for the Coder lane's DAC profile.

The profile is a frozen, plain-data description of how the Coder lane's
DAC agent should be built (`deepagents_code.agent.create_cli_agent`); it
carries no live model client, no secrets, and no I/O.
"""

import dataclasses

import pytest

from clipse_agent.profiles.coder import CoderProfile, get_coder_profile


def test_get_coder_profile_returns_a_coder_profile():
    profile = get_coder_profile()

    assert isinstance(profile, CoderProfile)


def test_coder_profile_is_frozen():
    profile = get_coder_profile()

    assert dataclasses.is_dataclass(profile)
    with pytest.raises(dataclasses.FrozenInstanceError):
        profile.model = "anthropic:some-other-model"


def test_get_coder_profile_is_deterministic():
    assert get_coder_profile() == get_coder_profile()


def test_get_coder_profile_model_override():
    assert get_coder_profile("openai_codex:gpt-5.5").model == "openai_codex:gpt-5.5"


def test_get_coder_profile_default_preserved():
    assert get_coder_profile().model == "anthropic:claude-sonnet-4-6"


def test_assistant_id_is_clipse_coder():
    profile = get_coder_profile()

    assert profile.assistant_id == "clipse-coder"


def test_model_is_a_provider_qualified_placeholder_not_a_key():
    profile = get_coder_profile()

    assert isinstance(profile.model, str)
    assert profile.model
    # DAC expects `provider:model` (deepagents_code.agent.create_cli_agent).
    assert ":" in profile.model
    provider, _, name = profile.model.partition(":")
    assert provider and name
    # Never a live credential — the model field is a spec string, not a key.
    assert not profile.model.lower().startswith("sk-")
    assert "key" not in profile.model.lower()


def test_system_prompt_covers_worktree_commits_and_stop_conditions():
    profile = get_coder_profile()

    assert isinstance(profile.system_prompt, str)
    prompt = profile.system_prompt.lower()
    assert profile.system_prompt.strip()
    assert "worktree" in prompt
    assert "commit" in prompt
    assert "issue" in prompt
    # Must stop on either terminal condition, not loop forever.
    assert "done" in prompt
    assert "blocked" in prompt


def test_system_prompt_defers_git_and_pr_to_the_platform():
    # The coder graph commits/pushes/opens the PR deterministically after the
    # DAC turn. If the prompt also tells DAC to open the PR itself, DAC runs a
    # `gh pr create` that the shell allow-list rejects (compound `cd && gh`)
    # and then LOOPS retrying variants until it burns the whole token budget
    # and blocks (observed live). So the prompt must hand git/gh (commit, push,
    # open PR) to the platform, leaving DAC to do file work only.
    profile = get_coder_profile()
    prompt = profile.system_prompt.lower()

    assert "platform" in prompt
    assert "pull request" in prompt
    # It must NOT instruct DAC to push/open the PR itself.
    assert "push your branch and open a pull request" not in prompt


def test_shell_allow_list_is_minimal_but_sufficient():
    profile = get_coder_profile()

    expected = {
        "git",
        "gh",
        "ls",
        "cat",
        "sed",
        "grep",
        "rg",
        "find",
        "mkdir",
        "go",
        "uv",
        "python",
        "python3",
        "make",
        "cd",
        "echo",
        "test",
    }

    assert set(profile.shell_allow_list) == expected
    # No duplicates and no blank entries snuck in.
    assert len(profile.shell_allow_list) == len(set(profile.shell_allow_list))
    assert all(isinstance(cmd, str) and cmd for cmd in profile.shell_allow_list)


def test_shell_allow_list_is_immutable():
    profile = get_coder_profile()

    with pytest.raises((AttributeError, TypeError)):
        profile.shell_allow_list.append("rm")
