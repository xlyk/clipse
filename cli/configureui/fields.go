package configureui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
)

type field struct {
	key       string
	label     string
	help      string
	page      pageID
	advanced  bool
	configKey bool
	options   []string
	input     textinput.Model
}

func newField(key, label, value string, page pageID, advanced, configKey bool, help string, options ...string) field {
	input := textinput.New()
	input.SetValue(value)
	input.CharLimit = 4096
	input.Width = 58
	input.Prompt = ""
	return field{key: key, label: label, help: help, page: page, advanced: advanced, configKey: configKey, options: options, input: input}
}

func newFields(draft setup.Draft, output string) []field {
	cfg := draft.Config
	worker, _ := json.Marshal(cfg.Worker.Command)
	return []field{
		newField("instance", "Instance name", draft.Instance, pageInstance, false, false, "Used to distinguish independent configs and runtime state."),
		newField("output", "Output file", output, pageInstance, false, false, "Secrets are never written to this YAML file."),
		newField("board_dir", "Board directory", cfg.BoardDir, pageInstance, false, true, "Must be unique for each concurrently running dispatcher."),
		newField("checkpoints_dir", "Checkpoint directory", cfg.CheckpointsDir, pageInstance, false, true, "Per-issue LangGraph checkpoint databases live here."),

		newField("repo.path", "Primary clone", cfg.Repo.Path, pageRepository, false, true, "Host Git operations still use this clone in Daytona mode."),
		newField("repo.remote", "GitHub remote", cfg.Repo.Remote, pageRepository, false, true, "Credential-free SSH or HTTPS GitHub URL."),
		newField("repo.base_branch", "Base branch", cfg.Repo.BaseBranch, pageRepository, false, true, "Workers branch from and PRs target this branch."),
		newField("repo.require_checks", "Require CI checks", strconv.FormatBool(cfg.Repo.RequireChecks), pageRepository, false, true, "Keep true unless this repository intentionally has no CI.", "true", "false"),

		newField("team_key", "Linear team key", cfg.TeamKey, pageLinear, false, true, "Press F4 to discover teams visible to LINEAR_API_KEY."),
		newField("team_id", "Linear team ID", cfg.TeamID, pageLinear, false, true, "Selected automatically during team discovery."),
		newField("lane_label_prefix", "Lane label prefix", cfg.LaneLabelPrefix, pageLinear, true, true, "agent:coder, agent:reviewer, and agent:git_operator opt work in."),
		newField("state_label_prefix", "State label prefix", cfg.StateLabelPrefix, pageLinear, false, true, "Leave blank for workflow states; use clipse: for label-backed state."),

		newField("agent_backend.type", "Agent backend", cfg.AgentBackend.Type, pageBackend, false, true, "Daytona is recommended; local is compatibility mode.", "daytona", "local"),
		newField("agent_backend.daytona.target", "Daytona target", cfg.AgentBackend.Daytona.Target, pageBackend, false, true, "Optional target/region. YAML wins over DAYTONA_TARGET."),
		newField("agent_backend.daytona.snapshot", "Daytona snapshot", cfg.AgentBackend.Daytona.Snapshot, pageBackend, false, true, "Optional snapshot name or ID containing the repo toolchain."),
		newField("agent_backend.daytona.auto_stop_minutes", "Coder auto-stop minutes", strconv.Itoa(cfg.AgentBackend.Daytona.AutoStopMinutes), pageBackend, true, true, "Idle coder sandboxes stop after this interval."),
		newField("agent_backend.daytona.reviewer_auto_delete_minutes", "Reviewer auto-delete minutes", strconv.Itoa(cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes), pageBackend, true, true, "Fallback cleanup timer for disposable reviewer sandboxes."),

		newField("models.coder", "Coder model", cfg.Models.Coder, pageModels, false, true, "provider:model"),
		newField("models.coder_docs", "Docs model", cfg.Models.CoderDocs, pageModels, false, true, "Best-effort documentation sub-turn inside the coder graph."),
		newField("models.reviewer", "Reviewer model", cfg.Models.Reviewer, pageModels, false, true, "Keep distinct from the coder when possible."),
		newField("model_params.coder", "Coder params JSON", marshalMap(cfg.ModelParams.Coder), pageModels, true, true, "Opaque model kwargs, for example {\"reasoning_effort\":\"high\"}."),
		newField("model_params.coder_docs", "Docs params JSON", marshalMap(cfg.ModelParams.CoderDocs), pageModels, true, true, "Blank inherits provider defaults."),
		newField("model_params.reviewer", "Reviewer params JSON", marshalMap(cfg.ModelParams.Reviewer), pageModels, true, true, "Blank inherits provider defaults."),

		newField("shell_allow_list.coder", "Coder shell", shellText(cfg.Shell.Coder), pageSafety, false, true, "all is unrestricted; otherwise enter comma-separated command names."),
		newField("shell_allow_list.coder_docs", "Docs shell", shellText(cfg.Shell.CoderDocs), pageSafety, false, true, "all is unrestricted; otherwise enter comma-separated command names."),
		newField("shell_allow_list.reviewer", "Reviewer shell", shellText(cfg.Shell.Reviewer), pageSafety, false, true, "all is unrestricted; otherwise enter comma-separated command names."),
		newField("env_allowlist", "Worker environment", strings.Join(cfg.EnvAllowlist, ","), pageSafety, true, true, "Never include LINEAR_API_KEY or Daytona controller variables."),

		newField("worker.command", "Worker argv JSON", string(worker), pageRuntime, false, true, "JSON array; the dispatcher appends per-run flags."),
		newField("poll_interval_s", "Poll interval seconds", strconv.Itoa(cfg.PollIntervalS), pageRuntime, true, true, "Linear polling cadence."),
		newField("caps.global", "Global concurrency", strconv.Itoa(cfg.Caps.Global), pageRuntime, true, true, "Maximum total running workers."),
		newField("caps.per_lane.coder", "Coder concurrency", strconv.Itoa(cfg.Caps.PerLane.Coder), pageRuntime, true, true, "Maximum concurrent coder runs."),
		newField("caps.per_lane.reviewer", "Reviewer concurrency", strconv.Itoa(cfg.Caps.PerLane.Reviewer), pageRuntime, true, true, "Maximum concurrent reviewer runs."),
		newField("caps.per_lane.git_operator", "Git operator concurrency", strconv.Itoa(cfg.Caps.PerLane.GitOperator), pageRuntime, true, true, "Deterministic merge-gate concurrency."),
		newField("turn_cap", "Turn cap", strconv.Itoa(cfg.TurnCap), pageRuntime, true, true, "Continuation bound per issue."),
		newField("max_runtime_s", "Max runtime seconds", strconv.Itoa(cfg.MaxRuntimeS), pageRuntime, true, true, "Wall-clock worker timeout."),
		newField("max_tokens_per_run", "Per-round token ceiling", strconv.Itoa(cfg.MaxTokensPerRun), pageRuntime, true, true, "Post-compaction context guard, not cumulative spend."),
		newField("max_attempts", "Max attempts", strconv.Itoa(cfg.MaxAttempts), pageRuntime, true, true, "Restart/orphan retry bound."),
		newField("rework_cap", "Rework cap", strconv.Itoa(cfg.ReworkCap), pageRuntime, true, true, "Coder-review cycle bound."),
		newField("recover_cap", "Recovery cap", strconv.Itoa(cfg.RecoverCap), pageRuntime, true, true, "Transient auto-recovery budget; zero disables."),
		newField("recover_backoff_s", "Recovery backoff seconds", strconv.Itoa(cfg.RecoverBackoffS), pageRuntime, true, true, "Delay before a transient retry can be claimed."),
	}
}

func marshalMap(value map[string]any) string {
	if value == nil {
		return ""
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}

func shellText(value config.ShellPolicy) string {
	if value.All {
		return "all"
	}
	return strings.Join(value.Commands, ",")
}

func (m Model) visibleFields() []int {
	indices := make([]int, 0)
	for i := range m.fields {
		f := m.fields[i]
		if f.page == m.page && (m.advanced || !f.advanced) {
			indices = append(indices, i)
		}
	}
	return indices
}

func (m *Model) syncFocus() {
	for i := range m.fields {
		m.fields[i].input.Blur()
	}
	visible := m.visibleFields()
	if len(visible) == 0 {
		m.cursor = 0
		return
	}
	if m.cursor >= len(visible) {
		m.cursor = len(visible) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if len(m.fields[visible[m.cursor]].options) == 0 {
		m.fields[visible[m.cursor]].input.Focus()
	}
}

func (m *Model) applyFields() error {
	cfg := m.draft.Config
	for i := range m.fields {
		key := m.fields[i].key
		value := strings.TrimSpace(m.fields[i].input.Value())
		var err error
		switch key {
		case "instance":
			m.draft.Instance = value
		case "output":
			m.outputPath = value
		case "repo.remote":
			cfg.Repo.Remote = value
		case "repo.path":
			cfg.Repo.Path = value
		case "repo.base_branch":
			cfg.Repo.BaseBranch = value
		case "repo.require_checks":
			cfg.Repo.RequireChecks, err = strconv.ParseBool(value)
		case "team_key":
			cfg.TeamKey = value
		case "team_id":
			cfg.TeamID = value
		case "lane_label_prefix":
			cfg.LaneLabelPrefix = value
		case "state_label_prefix":
			cfg.StateLabelPrefix = value
		case "agent_backend.type":
			cfg.AgentBackend.Type = value
		case "agent_backend.daytona.target":
			cfg.AgentBackend.Daytona.Target = value
		case "agent_backend.daytona.snapshot":
			cfg.AgentBackend.Daytona.Snapshot = value
		case "agent_backend.daytona.auto_stop_minutes":
			cfg.AgentBackend.Daytona.AutoStopMinutes, err = strconv.Atoi(value)
		case "agent_backend.daytona.reviewer_auto_delete_minutes":
			cfg.AgentBackend.Daytona.ReviewerAutoDeleteMinutes, err = strconv.Atoi(value)
		case "worker.command":
			err = json.Unmarshal([]byte(value), &cfg.Worker.Command)
		case "models.coder":
			cfg.Models.Coder = value
		case "models.coder_docs":
			cfg.Models.CoderDocs = value
		case "models.reviewer":
			cfg.Models.Reviewer = value
		case "model_params.coder":
			cfg.ModelParams.Coder, err = parseMap(value)
		case "model_params.coder_docs":
			cfg.ModelParams.CoderDocs, err = parseMap(value)
		case "model_params.reviewer":
			cfg.ModelParams.Reviewer, err = parseMap(value)
		case "shell_allow_list.coder":
			cfg.Shell.Coder, err = parseShell(value)
		case "shell_allow_list.coder_docs":
			cfg.Shell.CoderDocs, err = parseShell(value)
		case "shell_allow_list.reviewer":
			cfg.Shell.Reviewer, err = parseShell(value)
		case "env_allowlist":
			cfg.EnvAllowlist = csv(value)
		case "poll_interval_s":
			cfg.PollIntervalS, err = strconv.Atoi(value)
		case "caps.global":
			cfg.Caps.Global, err = strconv.Atoi(value)
		case "caps.per_lane.coder":
			cfg.Caps.PerLane.Coder, err = strconv.Atoi(value)
		case "caps.per_lane.reviewer":
			cfg.Caps.PerLane.Reviewer, err = strconv.Atoi(value)
		case "caps.per_lane.git_operator":
			cfg.Caps.PerLane.GitOperator, err = strconv.Atoi(value)
		case "turn_cap":
			cfg.TurnCap, err = strconv.Atoi(value)
		case "max_runtime_s":
			cfg.MaxRuntimeS, err = strconv.Atoi(value)
		case "max_tokens_per_run":
			cfg.MaxTokensPerRun, err = strconv.Atoi(value)
		case "max_attempts":
			cfg.MaxAttempts, err = strconv.Atoi(value)
		case "rework_cap":
			cfg.ReworkCap, err = strconv.Atoi(value)
		case "recover_cap":
			cfg.RecoverCap, err = strconv.Atoi(value)
		case "recover_backoff_s":
			cfg.RecoverBackoffS, err = strconv.Atoi(value)
		case "board_dir":
			cfg.BoardDir = value
		case "checkpoints_dir":
			cfg.CheckpointsDir = value
		}
		if err != nil {
			return fmt.Errorf("%s: %w", m.fields[i].label, err)
		}
	}
	m.draft.Config = cfg
	return nil
}

func parseMap(value string) (map[string]any, error) {
	if value == "" {
		return nil, nil
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil, err
	}
	return result, nil
}

func parseShell(value string) (config.ShellPolicy, error) {
	if value == "all" {
		return config.ShellPolicy{All: true}, nil
	}
	commands := csv(value)
	if len(commands) == 0 {
		return config.ShellPolicy{}, fmt.Errorf("use all or a comma-separated command list")
	}
	return config.ShellPolicy{Commands: commands}, nil
}

func csv(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if item := strings.TrimSpace(part); item != "" {
			result = append(result, item)
		}
	}
	return result
}
