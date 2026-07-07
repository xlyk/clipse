"""Tests for the Coder lane's DAC profile.

The profile is a frozen, plain-data description of how the Coder lane's
DAC agent should be built (`deepagents_code.agent.create_cli_agent`); it
carries no live model client, no secrets, and no I/O.
"""

import dataclasses

import pytest

from clipse_agent.profiles.coder import _SHELL_ALLOW_LIST, CoderProfile, get_coder_profile


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


def test_get_coder_profile_model_params_override():
    profile = get_coder_profile(model_params={"reasoning_effort": "high"})

    assert profile.model_params == {"reasoning_effort": "high"}


def test_get_coder_profile_model_params_default_is_none():
    assert get_coder_profile().model_params is None


def test_get_coder_profile_context_window_tokens_default():
    assert get_coder_profile().context_window_tokens == 200_000


def test_get_coder_profile_context_window_tokens_override():
    profile = get_coder_profile(context_window_tokens=100_000)

    assert profile.context_window_tokens == 100_000


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


def test_system_prompt_nudges_compact_conversation_tool():
    # Option 5 (belt-and-suspenders): DAC's compact_conversation tool never
    # auto-fires (it's model-invoked only), so the prompt nudges the model to
    # call it proactively -- complementary to the auto-summarizer trigger
    # lowered in dac.build_coder_agent, not a replacement for it.
    profile = get_coder_profile()

    assert "compact_conversation" in profile.system_prompt


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


def test_get_coder_profile_defaults_to_unrestricted_shell():
    # New default (decision 2026-07-07): an unconfigured lane runs with no
    # shell allow-list at all -- dac.build_coder_agent maps this to DAC's
    # auto_approve=True, matching the kernel's own `all`-policy default
    # (internal/config's shell_allow_list).
    assert get_coder_profile().shell_allow_list is None


def test_get_coder_profile_shell_allow_list_override():
    profile = get_coder_profile(shell_allow_list=["git", "gh"])

    # Stored as a tuple (not the list passed in) so the frozen dataclass
    # stays immutable end to end, mirroring the factory's own reference list.
    assert profile.shell_allow_list == ("git", "gh")
    assert isinstance(profile.shell_allow_list, tuple)


def test_shell_allow_list_is_minimal_but_sufficient():
    # Restrictive mode: a caller (the kernel, via worker.py) that opts into
    # an explicit allow-list gets back exactly what it passed, still
    # matching the reference list this module exports.
    profile = get_coder_profile(shell_allow_list=_SHELL_ALLOW_LIST)

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
    profile = get_coder_profile(shell_allow_list=_SHELL_ALLOW_LIST)

    with pytest.raises((AttributeError, TypeError)):
        profile.shell_allow_list.append("rm")
