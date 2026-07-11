package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/config"
)

// writeYAML writes contents to a temp file named clipse.yaml and returns its path.
func writeYAML(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture yaml: %v", err)
	}
	return path
}

// baseValidYAML is a minimal valid config (mirrors
// TestLoad_MinimalConfigGetsDefaults's fixture) that tests needing a
// loadable base append field-specific overrides to.
const baseValidYAML = `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - clipse-worker
`

// loadYAML writes contents to a temp clipse.yaml and loads it, failing the
// test immediately on any unexpected Load error.
func loadYAML(t *testing.T, contents string) *config.Config {
	t.Helper()
	path := writeYAML(t, contents)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	return cfg
}

func TestLoad_ValidFullConfig(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
poll_interval_s: 45
caps:
  global: 10
  per_lane:
    coder: 5
    reviewer: 3
    git_operator: 2
turn_cap: 7
max_runtime_s: 1800
rework_cap: 6
recover_cap: 5
recover_backoff_s: 90
max_tokens_per_run: 250000
lane_label_prefix: "lane:"
max_attempts: 4
env_allowlist:
  - ANTHROPIC_API_KEY
  - PATH
  - HOME
  - GH_TOKEN
worker:
  command:
    - uv
    - --project
    - /abs/path/agent
    - run
    - clipse-worker
models:
  coder: "openai_codex:gpt-5.5"
  coder_docs: "openai_codex:gpt-5.5-mini"
  reviewer: "anthropic:claude-opus-4-7"
checkpoints_dir: "/abs/path/board/checkpoints"
board_dir: "/abs/path/board"
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if cfg.Repo.Remote != "https://github.com/yourorg/yourrepo.git" {
		t.Errorf("Repo.Remote = %q, want %q", cfg.Repo.Remote, "https://github.com/yourorg/yourrepo.git")
	}
	if cfg.Repo.Path != "/home/you/code/yourrepo" {
		t.Errorf("Repo.Path = %q, want %q", cfg.Repo.Path, "/home/you/code/yourrepo")
	}
	if cfg.Repo.BaseBranch != "main" {
		t.Errorf("Repo.BaseBranch = %q, want %q", cfg.Repo.BaseBranch, "main")
	}
	if cfg.TeamKey != "CLI" {
		t.Errorf("TeamKey = %q, want %q", cfg.TeamKey, "CLI")
	}
	if cfg.TeamID != "8b5b3301-8da3-4933-9b07-9efc027bc09d" {
		t.Errorf("TeamID = %q, want %q", cfg.TeamID, "8b5b3301-8da3-4933-9b07-9efc027bc09d")
	}
	if cfg.PollIntervalS != 45 {
		t.Errorf("PollIntervalS = %d, want 45", cfg.PollIntervalS)
	}
	if cfg.Caps.Global != 10 {
		t.Errorf("Caps.Global = %d, want 10", cfg.Caps.Global)
	}
	if cfg.Caps.PerLane.Coder != 5 {
		t.Errorf("Caps.PerLane.Coder = %d, want 5", cfg.Caps.PerLane.Coder)
	}
	if cfg.Caps.PerLane.Reviewer != 3 {
		t.Errorf("Caps.PerLane.Reviewer = %d, want 3", cfg.Caps.PerLane.Reviewer)
	}
	if cfg.Caps.PerLane.GitOperator != 2 {
		t.Errorf("Caps.PerLane.GitOperator = %d, want 2", cfg.Caps.PerLane.GitOperator)
	}
	if cfg.TurnCap != 7 {
		t.Errorf("TurnCap = %d, want 7", cfg.TurnCap)
	}
	if cfg.MaxRuntimeS != 1800 {
		t.Errorf("MaxRuntimeS = %d, want 1800", cfg.MaxRuntimeS)
	}
	if cfg.ReworkCap != 6 {
		t.Errorf("ReworkCap = %d, want 6", cfg.ReworkCap)
	}
	if cfg.RecoverCap != 5 {
		t.Errorf("RecoverCap = %d, want 5", cfg.RecoverCap)
	}
	if cfg.RecoverBackoffS != 90 {
		t.Errorf("RecoverBackoffS = %d, want 90", cfg.RecoverBackoffS)
	}
	if cfg.LaneLabelPrefix != "lane:" {
		t.Errorf("LaneLabelPrefix = %q, want %q", cfg.LaneLabelPrefix, "lane:")
	}
	if cfg.MaxAttempts != 4 {
		t.Errorf("MaxAttempts = %d, want 4", cfg.MaxAttempts)
	}
	wantAllowlist := []string{"ANTHROPIC_API_KEY", "PATH", "HOME", "GH_TOKEN"}
	if !slices.Equal(cfg.EnvAllowlist, wantAllowlist) {
		t.Errorf("EnvAllowlist = %v, want %v", cfg.EnvAllowlist, wantAllowlist)
	}
	if cfg.MaxTokensPerRun != 250000 {
		t.Errorf("MaxTokensPerRun = %d, want 250000", cfg.MaxTokensPerRun)
	}
	wantCommand := []string{"uv", "--project", "/abs/path/agent", "run", "clipse-worker"}
	if !slices.Equal(cfg.Worker.Command, wantCommand) {
		t.Errorf("Worker.Command = %v, want %v", cfg.Worker.Command, wantCommand)
	}
	if cfg.CheckpointsDir != "/abs/path/board/checkpoints" {
		t.Errorf("CheckpointsDir = %q, want %q", cfg.CheckpointsDir, "/abs/path/board/checkpoints")
	}
	if cfg.BoardDir != "/abs/path/board" {
		t.Errorf("BoardDir = %q, want %q", cfg.BoardDir, "/abs/path/board")
	}
	if cfg.Models.Coder != "openai_codex:gpt-5.5" {
		t.Errorf("Models.Coder = %q, want %q", cfg.Models.Coder, "openai_codex:gpt-5.5")
	}
	if cfg.Models.CoderDocs != "openai_codex:gpt-5.5-mini" {
		t.Errorf("Models.CoderDocs = %q, want %q", cfg.Models.CoderDocs, "openai_codex:gpt-5.5-mini")
	}
	if cfg.Models.Reviewer != "anthropic:claude-opus-4-7" {
		t.Errorf("Models.Reviewer = %q, want %q", cfg.Models.Reviewer, "anthropic:claude-opus-4-7")
	}
}

func TestLoad_MinimalConfigGetsDefaults(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - clipse-worker
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if cfg.PollIntervalS != 30 {
		t.Errorf("PollIntervalS = %d, want default 30", cfg.PollIntervalS)
	}
	if cfg.Caps.Global != 8 {
		t.Errorf("Caps.Global = %d, want default 8", cfg.Caps.Global)
	}
	if cfg.Caps.PerLane.Coder != 4 {
		t.Errorf("Caps.PerLane.Coder = %d, want default 4", cfg.Caps.PerLane.Coder)
	}
	if cfg.Caps.PerLane.Reviewer != 2 {
		t.Errorf("Caps.PerLane.Reviewer = %d, want default 2", cfg.Caps.PerLane.Reviewer)
	}
	if cfg.Caps.PerLane.GitOperator != 1 {
		t.Errorf("Caps.PerLane.GitOperator = %d, want default 1", cfg.Caps.PerLane.GitOperator)
	}
	if cfg.TurnCap != 5 {
		t.Errorf("TurnCap = %d, want default 5", cfg.TurnCap)
	}
	if cfg.MaxRuntimeS != 3600 {
		t.Errorf("MaxRuntimeS = %d, want default 3600", cfg.MaxRuntimeS)
	}
	if cfg.ReworkCap != 3 {
		t.Errorf("ReworkCap = %d, want default 3", cfg.ReworkCap)
	}
	if cfg.RecoverCap != 5 {
		t.Errorf("RecoverCap = %d, want default 5", cfg.RecoverCap)
	}
	// recover_backoff_s defaults to the resolved poll_interval_s (30 here,
	// since poll_interval_s is also absent from this minimal config).
	if cfg.RecoverBackoffS != 30 {
		t.Errorf("RecoverBackoffS = %d, want default 30 (poll_interval_s)", cfg.RecoverBackoffS)
	}
	if cfg.LaneLabelPrefix != "agent:" {
		t.Errorf("LaneLabelPrefix = %q, want default %q", cfg.LaneLabelPrefix, "agent:")
	}
	if cfg.MaxAttempts != 3 {
		t.Errorf("MaxAttempts = %d, want default 3", cfg.MaxAttempts)
	}
	wantDefaultAllowlist := []string{"ANTHROPIC_API_KEY", "PATH", "HOME", "GH_TOKEN", "GITHUB_TOKEN", "TESTWORKER_SCENARIO"}
	if !slices.Equal(cfg.EnvAllowlist, wantDefaultAllowlist) {
		t.Errorf("EnvAllowlist = %v, want default %v", cfg.EnvAllowlist, wantDefaultAllowlist)
	}
	if cfg.MaxTokensPerRun != 400000 {
		t.Errorf("MaxTokensPerRun = %d, want default 400000", cfg.MaxTokensPerRun)
	}
	if cfg.CheckpointsDir != "./.clipse/checkpoints" {
		t.Errorf("CheckpointsDir = %q, want default %q", cfg.CheckpointsDir, "./.clipse/checkpoints")
	}
	if cfg.BoardDir != "./.clipse" {
		t.Errorf("BoardDir = %q, want default %q", cfg.BoardDir, "./.clipse")
	}
	if cfg.Models.Coder != "anthropic:claude-sonnet-4-6" {
		t.Errorf("Models.Coder = %q, want default %q", cfg.Models.Coder, "anthropic:claude-sonnet-4-6")
	}
	if cfg.Models.CoderDocs != "anthropic:claude-sonnet-4-6" {
		t.Errorf("Models.CoderDocs = %q, want default %q", cfg.Models.CoderDocs, "anthropic:claude-sonnet-4-6")
	}
	if cfg.Models.Reviewer != "anthropic:claude-opus-4-6" {
		t.Errorf("Models.Reviewer = %q, want default %q", cfg.Models.Reviewer, "anthropic:claude-opus-4-6")
	}
	// repo.require_checks defaults to true (the safe default: absent checks
	// wait for CI to register rather than proceeding to merge).
	if !cfg.Repo.RequireChecks {
		t.Errorf("Repo.RequireChecks = %v, want default true", cfg.Repo.RequireChecks)
	}
}

func TestLoad_AgentBackendDefaultsToLocal(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML)
	if cfg.AgentBackend.Type != "local" {
		t.Fatalf("AgentBackend.Type = %q, want local", cfg.AgentBackend.Type)
	}
}

func TestLoad_DaytonaBackendDefaults(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML+`
agent_backend:
  type: daytona
`)
	if cfg.AgentBackend.Daytona.AutoStopMinutes != 60 {
		t.Errorf("AutoStopMinutes = %d, want 60", cfg.AgentBackend.Daytona.AutoStopMinutes)
	}
	if cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes != 60 {
		t.Errorf("ReviewerAutoDeleteMinutes = %d, want 60", cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes)
	}
}

func TestLoad_DaytonaBackendRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantErrSubstr string
	}{
		{
			name: "unsupported backend type",
			yaml: baseValidYAML + `
agent_backend:
  type: docker
`,
			wantErrSubstr: "agent_backend.type",
		},
		{
			name: "zero auto stop interval",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    auto_stop_minutes: 0
`,
			wantErrSubstr: "agent_backend.daytona.auto_stop_minutes",
		},
		{
			name: "negative auto stop interval",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    auto_stop_minutes: -1
`,
			wantErrSubstr: "agent_backend.daytona.auto_stop_minutes",
		},
		{
			name: "zero reviewer auto delete interval",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    reviewer_auto_delete_minutes: 0
`,
			wantErrSubstr: "agent_backend.daytona.reviewer_auto_delete_minutes",
		},
		{
			name: "negative reviewer auto delete interval",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    reviewer_auto_delete_minutes: -1
`,
			wantErrSubstr: "agent_backend.daytona.reviewer_auto_delete_minutes",
		},
		{
			name: "explicitly empty snapshot",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    snapshot: ""
`,
			wantErrSubstr: "agent_backend.daytona.snapshot",
		},
		{
			name: "explicitly empty target",
			yaml: baseValidYAML + `
agent_backend:
  type: daytona
  daytona:
    target: ""
`,
			wantErrSubstr: "agent_backend.daytona.target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeYAML(t, tt.yaml)

			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("Load: expected error containing %q, got nil", tt.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("Load: error %q does not mention %q", err.Error(), tt.wantErrSubstr)
			}
		})
	}
}

func TestLoad_RejectsDaytonaVariablesInGeneralEnvAllowlist(t *testing.T) {
	for _, key := range []string{"DAYTONA_API_KEY", "DAYTONA_API_URL", "DAYTONA_TARGET"} {
		t.Run(key, func(t *testing.T) {
			path := writeYAML(t, baseValidYAML+`
env_allowlist:
  - PATH
  - `+key+`
`)
			_, err := config.Load(path)
			if err == nil {
				t.Fatal("Load error = nil")
			}
			if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "env_allowlist") {
				t.Errorf("Load error = %q, want safe env_allowlist rejection naming %s", err, key)
			}
		})
	}
}

func TestLoad_RejectsCredentialBearingRepoRemoteWithoutLeakingIt(t *testing.T) {
	path := writeYAML(t, strings.Replace(
		baseValidYAML,
		"https://github.com/yourorg/yourrepo.git",
		"https://user:token-secret@github.com/x/y.git",
		1,
	))
	_, err := config.Load(path)
	if err == nil {
		t.Fatal("Load error = nil")
	}
	if !strings.Contains(err.Error(), "repo.remote") {
		t.Errorf("Load error = %q, want repo.remote", err)
	}
	for _, forbidden := range []string{"user", "token-secret", "https://"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("Load error %q leaked rejected remote content %q", err, forbidden)
		}
	}
}

// TestLoad_RequireChecksExplicitFalse asserts an explicit repo.require_checks:
// false is honored (a repo declaring it has no CI at all), distinct from the
// absent-key default of true.
func TestLoad_RequireChecksExplicitFalse(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
  require_checks: false
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - clipse-worker
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.Repo.RequireChecks {
		t.Errorf("Repo.RequireChecks = %v, want explicit false", cfg.Repo.RequireChecks)
	}
}

// TestLoad_RecoverBackoffDefaultsToPollInterval asserts recover_backoff_s
// tracks a *custom* poll_interval_s when the former is absent — the retry
// backoff should land roughly one poll later regardless of the poll cadence.
func TestLoad_RecoverBackoffDefaultsToPollInterval(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
poll_interval_s: 60
worker:
  command:
    - clipse-worker
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.RecoverBackoffS != 60 {
		t.Errorf("RecoverBackoffS = %d, want 60 (follows custom poll_interval_s)", cfg.RecoverBackoffS)
	}
}

// TestLoad_RecoverCapZeroIsValid asserts a recover_cap of 0 loads cleanly: it
// is the documented kill switch that disables auto-recovery entirely, unlike
// max_attempts/rework_cap which must be >= 1.
func TestLoad_RecoverCapZeroIsValid(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
recover_cap: 0
worker:
  command:
    - clipse-worker
`)

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.RecoverCap != 0 {
		t.Errorf("RecoverCap = %d, want 0 (kill switch)", cfg.RecoverCap)
	}
}

// TestLoad_PartialModelsOverride asserts models.* fields default
// independently: overriding only models.coder must not disturb
// coder_docs/reviewer's own defaults.
func TestLoad_PartialModelsOverride(t *testing.T) {
	// only models.coder set -> coder_docs & reviewer keep their independent defaults
	cfg := loadYAML(t, baseValidYAML+`
models:
  coder: "openai_codex:gpt-5.5"
`)
	if cfg.Models.Coder != "openai_codex:gpt-5.5" {
		t.Errorf("Coder = %q, want openai_codex:gpt-5.5", cfg.Models.Coder)
	}
	if cfg.Models.CoderDocs != "anthropic:claude-sonnet-4-6" {
		t.Errorf("CoderDocs = %q, want default", cfg.Models.CoderDocs)
	}
	if cfg.Models.Reviewer != "anthropic:claude-opus-4-6" {
		t.Errorf("Reviewer = %q, want default", cfg.Models.Reviewer)
	}
}

// TestLoad_ModelParams asserts a model_params: block parses into opaque
// per-lane maps (including nested values), that an omitted model_params
// yields nil maps for all three lanes, and that keys default independently
// (only coder set -> coder_docs/reviewer stay nil).
func TestLoad_ModelParams(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML+`
model_params:
  coder: { reasoning_effort: high }
  reviewer: { thinking: { type: enabled, budget_tokens: 10000 } }
`)
	if cfg.ModelParams.Coder["reasoning_effort"] != "high" {
		t.Errorf("coder reasoning_effort = %v", cfg.ModelParams.Coder["reasoning_effort"])
	}
	if cfg.ModelParams.CoderDocs != nil {
		t.Errorf("coder_docs should be nil, got %v", cfg.ModelParams.CoderDocs)
	}
	th, _ := cfg.ModelParams.Reviewer["thinking"].(map[string]any)
	if th == nil || th["type"] != "enabled" {
		t.Errorf("reviewer thinking = %v", cfg.ModelParams.Reviewer["thinking"])
	}
}

// TestLoad_ModelParamsOmittedIsNil asserts that omitting model_params
// entirely yields nil maps for every lane — no defaulting, since
// model_params is an opaque passthrough with no built-in values.
func TestLoad_ModelParamsOmittedIsNil(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML)

	if cfg.ModelParams.Coder != nil {
		t.Errorf("Coder = %v, want nil", cfg.ModelParams.Coder)
	}
	if cfg.ModelParams.CoderDocs != nil {
		t.Errorf("CoderDocs = %v, want nil", cfg.ModelParams.CoderDocs)
	}
	if cfg.ModelParams.Reviewer != nil {
		t.Errorf("Reviewer = %v, want nil", cfg.ModelParams.Reviewer)
	}
}

// TestShellPolicy_Defaults_AllWhenAbsent asserts every lane's shell policy
// defaults to All when shell_allow_list is absent from the YAML document
// entirely — the decision-2026-07-07 safe default (reflex's live-fire
// showed the restrictive mode's hardcoded pattern checks rejecting ~55
// legitimate commands across 25 issues).
func TestShellPolicy_Defaults_AllWhenAbsent(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML)
	for name, p := range map[string]config.ShellPolicy{
		"coder": cfg.Shell.Coder, "coder_docs": cfg.Shell.CoderDocs, "reviewer": cfg.Shell.Reviewer,
	} {
		if !p.All || len(p.Commands) != 0 {
			t.Fatalf("%s: expected default All policy, got %+v", name, p)
		}
	}
}

// TestShellPolicy_ParsesScalarAllAndList asserts the custom
// ShellPolicy.UnmarshalYAML accepts both forms in the same document: the
// scalar sentinel `all` and an explicit command-name sequence. coder_docs is
// omitted entirely, so it must independently default to All.
func TestShellPolicy_ParsesScalarAllAndList(t *testing.T) {
	cfg := loadYAML(t, baseValidYAML+`
shell_allow_list:
  coder: all
  reviewer: [git, gh, ls]
`)
	if !cfg.Shell.Coder.All {
		t.Errorf("Shell.Coder.All = %v, want true", cfg.Shell.Coder.All)
	}
	if len(cfg.Shell.Coder.Commands) != 0 {
		t.Errorf("Shell.Coder.Commands = %v, want empty", cfg.Shell.Coder.Commands)
	}
	if cfg.Shell.Reviewer.All {
		t.Errorf("Shell.Reviewer.All = %v, want false", cfg.Shell.Reviewer.All)
	}
	wantReviewerCommands := []string{"git", "gh", "ls"}
	if !slices.Equal(cfg.Shell.Reviewer.Commands, wantReviewerCommands) {
		t.Errorf("Shell.Reviewer.Commands = %v, want %v", cfg.Shell.Reviewer.Commands, wantReviewerCommands)
	}
	if !cfg.Shell.CoderDocs.All {
		t.Errorf("Shell.CoderDocs.All = %v, want default true (omitted from shell_allow_list)", cfg.Shell.CoderDocs.All)
	}
	if len(cfg.Shell.CoderDocs.Commands) != 0 {
		t.Errorf("Shell.CoderDocs.Commands = %v, want empty", cfg.Shell.CoderDocs.Commands)
	}
}

// TestShellPolicy_RejectsEmptyListAndAllInList asserts two distinct
// validate-time errors: an explicit empty list (`coder: []`) is NOT silently
// treated as All — defaultShellPolicy only defaults an absent key, not an
// explicit-but-empty one (see its doc comment) — and a list containing the
// `all` sentinel as an element is rejected in favor of the scalar form.
func TestShellPolicy_RejectsEmptyListAndAllInList(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantErrSubstr string
	}{
		{
			name: "explicit empty list",
			yaml: baseValidYAML + `
shell_allow_list:
  coder: []
`,
			wantErrSubstr: "shell_allow_list.coder",
		},
		{
			name: "all as a list element",
			yaml: baseValidYAML + `
shell_allow_list:
  coder: [all, git]
`,
			wantErrSubstr: "shell_allow_list.coder",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeYAML(t, tt.yaml)

			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("Load: expected error containing %q, got nil", tt.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("Load: error %q does not mention %q", err.Error(), tt.wantErrSubstr)
			}
		})
	}
}

func TestLoad_InvalidConfigs(t *testing.T) {
	tests := []struct {
		name          string
		yaml          string
		wantErrSubstr string
	}{
		{
			name: "missing repo.remote",
			yaml: `
repo:
  path: "/home/you/code/yourrepo"
  base_branch: "main"
`,
			wantErrSubstr: "repo.remote",
		},
		{
			name: "missing repo.path",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  base_branch: "main"
`,
			wantErrSubstr: "repo.path",
		},
		{
			name: "missing repo.base_branch",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
`,
			wantErrSubstr: "repo.base_branch",
		},
		{
			name: "non-positive poll_interval_s",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
poll_interval_s: 0
`,
			wantErrSubstr: "poll_interval_s",
		},
		{
			name: "non-positive turn_cap",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
turn_cap: -1
`,
			wantErrSubstr: "turn_cap",
		},
		{
			name: "non-positive max_runtime_s",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
max_runtime_s: 0
`,
			wantErrSubstr: "max_runtime_s",
		},
		{
			name: "negative caps.global",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
caps:
  global: -1
`,
			wantErrSubstr: "caps.global",
		},
		{
			name: "caps.global less than 1",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
caps:
  global: 0
`,
			wantErrSubstr: "caps.global",
		},
		{
			name: "negative caps.per_lane.coder",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
caps:
  per_lane:
    coder: -1
`,
			wantErrSubstr: "caps.per_lane.coder",
		},
		{
			name: "non-positive max_attempts",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
max_attempts: 0
`,
			wantErrSubstr: "max_attempts",
		},
		{
			name: "non-positive rework_cap",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
rework_cap: 0
`,
			wantErrSubstr: "rework_cap",
		},
		{
			name: "missing team_key",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
`,
			wantErrSubstr: "team_key",
		},
		{
			name: "missing team_id",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
`,
			wantErrSubstr: "team_id",
		},
		{
			// A worker must NEVER see LINEAR_API_KEY (kernel-only secret,
			// threat model B3) — reject it at config-load time rather than
			// silently forwarding it into every spawned worker's env.
			name: "env_allowlist contains LINEAR_API_KEY",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
env_allowlist:
  - ANTHROPIC_API_KEY
  - LINEAR_API_KEY
`,
			wantErrSubstr: "LINEAR_API_KEY",
		},
		{
			name: "explicitly empty env_allowlist",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
env_allowlist: []
`,
			wantErrSubstr: "env_allowlist",
		},
		{
			// worker.command is the argv PREFIX the Spawner execs for every
			// worker invocation (e.g. a uv-scoped run of clipse-worker); it
			// is machine-specific (an absolute --project path in the common
			// case), so unlike most fields it has no default — Load requires
			// it explicitly. team_key/team_id are set here (unlike the other
			// "missing repo.*" cases above) because validate checks
			// worker.command LAST, after every pre-existing check — omitting
			// them would make this fixture fail on "team_key is required"
			// first instead of isolating worker.command.
			name: "missing worker.command",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
`,
			wantErrSubstr: "worker.command",
		},
		{
			name: "empty worker.command",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command: []
`,
			wantErrSubstr: "worker.command",
		},
		{
			name: "worker.command contains an empty entry",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - uv
    - ""
`,
			wantErrSubstr: "worker.command",
		},
		{
			name: "negative recover_cap",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
recover_cap: -1
`,
			wantErrSubstr: "recover_cap",
		},
		{
			name: "negative recover_backoff_s",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
recover_backoff_s: -5
`,
			wantErrSubstr: "recover_backoff_s",
		},
		{
			name: "non-positive max_tokens_per_run",
			yaml: `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - clipse-worker
max_tokens_per_run: 0
`,
			wantErrSubstr: "max_tokens_per_run",
		},
		{
			name: "model missing colon",
			yaml: baseValidYAML + `
models:
  coder: "gpt-5.5"
`,
			wantErrSubstr: "models.coder",
		},
		{
			name: "model empty provider",
			yaml: baseValidYAML + `
models:
  coder: ":gpt-5.5"
`,
			wantErrSubstr: "models.coder",
		},
		{
			name: "model empty name",
			yaml: baseValidYAML + `
models:
  coder: "openai_codex:"
`,
			wantErrSubstr: "models.coder",
		},
		{
			name: "shell_allow_list scalar not all",
			yaml: baseValidYAML + `
shell_allow_list:
  coder: everything
`,
			wantErrSubstr: "shell_allow_list",
		},
		{
			name: "shell_allow_list list contains an empty entry",
			yaml: baseValidYAML + `
shell_allow_list:
  reviewer: [git, ""]
`,
			wantErrSubstr: "shell_allow_list.reviewer",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeYAML(t, tt.yaml)

			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("Load: expected error containing %q, got nil", tt.wantErrSubstr)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstr) {
				t.Errorf("Load: error %q does not mention %q", err.Error(), tt.wantErrSubstr)
			}
		})
	}
}

func TestLoad_ExampleConfigLoadsWithoutError(t *testing.T) {
	// configs/clipse.example.yaml is the canonical shape shipped in the repo;
	// it must always parse and validate cleanly.
	path := filepath.Join("..", "..", "configs", "clipse.example.yaml")

	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load(%q): unexpected error: %v", path, err)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.yaml")

	if _, err := config.Load(path); err == nil {
		t.Fatal("Load: expected error for missing file, got nil")
	}
}
