package configureui_test

import (
	"context"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/cli/configureui"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
)

func testModel(t *testing.T) configureui.Model {
	t.Helper()
	draft := setup.NewDraft("test", "/opt/clipse", t.TempDir())
	draft.Config.Repo.Remote = "git@github.com:acme/widget.git"
	draft.Config.Repo.Path = "/opt/widget"
	draft.Config.Repo.BaseBranch = "main"
	draft.Config.TeamKey = "WID"
	draft.Config.TeamID = "team-id"
	return configureui.NewModel(configureui.Options{
		Draft:       draft,
		OutputPath:  filepath.Join(t.TempDir(), "clipse.yaml"),
		Advanced:    true,
		NoColor:     true,
		NoAnimation: true,
	})
}

func TestWizardCompletesReviewAndWriteWithInjectedServices(t *testing.T) {
	draft := setup.NewDraft("test", "/opt/clipse", t.TempDir())
	draft.Config.Repo.Remote = "git@github.com:acme/widget.git"
	draft.Config.Repo.Path = "/opt/widget"
	draft.Config.Repo.BaseBranch = "main"
	draft.Config.TeamKey = "WID"
	draft.Config.TeamID = "team-id"
	output := filepath.Join(t.TempDir(), "clipse.yaml")
	m := configureui.NewModel(configureui.Options{
		Draft:       draft,
		OutputPath:  output,
		NoColor:     true,
		NoAnimation: true,
		Services: configureui.Services{
			Check: func(context.Context, config.Config) setup.Report {
				return setup.Report{Outcome: setup.OutcomeReady, Results: []setup.CheckResult{{ID: "config", Severity: setup.SeverityPass, Summary: "ready"}}}
			},
			Write: func(path string, _ []byte, _ setup.WriteOptions) (setup.WriteResult, error) {
				return setup.WriteResult{Path: path}, nil
			},
		},
	})

	for steps := 0; m.PageName() != "REVIEW" && steps < 100; steps++ {
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		m = next.(configureui.Model)
		if cmd != nil {
			next, _ = m.Update(cmd())
			m = next.(configureui.Model)
		}
	}
	if m.PageName() != "REVIEW" {
		t.Fatalf("wizard did not reach Review; page=%s", m.PageName())
	}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}})
	m = next.(configureui.Model)
	if cmd == nil {
		t.Fatal("Review write returned no command")
	}
	next, _ = m.Update(cmd())
	m = next.(configureui.Model)
	if got := m.Result().WrittenPath; got != output {
		t.Fatalf("WrittenPath = %q, want %q", got, output)
	}
}

func TestModelCoversEveryConfigField(t *testing.T) {
	m := testModel(t)
	got := make(map[string]bool)
	for _, key := range m.ConfigFieldKeys() {
		got[key] = true
	}
	want := []string{
		"repo.remote", "repo.path", "repo.base_branch", "repo.require_checks",
		"team_key", "team_id", "lane_label_prefix", "state_label_prefix",
		"agent_backend.type", "agent_backend.daytona.auto_stop_minutes",
		"agent_backend.daytona.reviewer_auto_delete_minutes", "agent_backend.daytona.snapshot", "agent_backend.daytona.target",
		"worker.command", "models.coder", "models.coder_docs", "models.reviewer",
		"model_params.coder", "model_params.coder_docs", "model_params.reviewer",
		"shell_allow_list.coder", "shell_allow_list.coder_docs", "shell_allow_list.reviewer",
		"env_allowlist", "poll_interval_s", "caps.global", "caps.per_lane.coder",
		"caps.per_lane.reviewer", "caps.per_lane.git_operator", "turn_cap", "max_runtime_s",
		"max_tokens_per_run", "max_attempts", "rework_cap", "recover_cap", "recover_backoff_s",
		"board_dir", "checkpoints_dir",
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("wizard has no field for %s", key)
		}
	}
}

func TestModelCancelNeverWrites(t *testing.T) {
	m := testModel(t)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	got := next.(configureui.Model)
	if !got.Result().Canceled {
		t.Fatal("Ctrl+C did not mark the wizard canceled")
	}
	if got.Result().WrittenPath != "" {
		t.Fatalf("canceled wizard wrote %q", got.Result().WrittenPath)
	}
}

func TestNarrowMonochromeViewStillShowsCurrentField(t *testing.T) {
	m := testModel(t)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 58, Height: 18})
	m = next.(configureui.Model)
	view := m.View()
	if view == "" {
		t.Fatal("narrow View is empty")
	}
	if !contains(view, "INSTANCE") || !contains(view, "Output file") {
		t.Fatalf("narrow View missing current step/field:\n%s", view)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
