package setup_test

import (
	"bytes"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
)

func validConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Defaults()
	cfg.Repo = config.Repo{
		Remote:        "git@github.com:acme/widget.git",
		Path:          filepath.Join(t.TempDir(), "widget"),
		BaseBranch:    "main",
		RequireChecks: true,
	}
	cfg.AgentBackend.Type = "daytona"
	cfg.AgentBackend.Daytona.Target = "us"
	cfg.TeamKey = "WID"
	cfg.TeamID = "team-id"
	cfg.StateLabelPrefix = "clipse:"
	cfg.Worker.Command = []string{"uv", "--project", "/opt/clipse/agent", "run", "clipse-worker"}
	cfg.BoardDir = filepath.Join(t.TempDir(), "board")
	cfg.CheckpointsDir = filepath.Join(cfg.BoardDir, "checkpoints")
	return cfg
}

func TestRenderRoundTripsCompleteConfig(t *testing.T) {
	cfg := validConfig(t)
	cfg.ModelParams.Coder = map[string]any{"reasoning_effort": "high"}
	cfg.Shell.Reviewer = config.ShellPolicy{Commands: []string{"git", "gh", "rg"}}

	raw, err := setup.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, forbidden := range [][]byte{[]byte("LINEAR_API_KEY"), []byte("DAYTONA_API_KEY"), []byte("super-secret")} {
		if bytes.Contains(raw, forbidden) {
			t.Fatalf("rendered config contains forbidden value %q", forbidden)
		}
	}

	parsed, err := config.Parse(raw, "rendered-test")
	if err != nil {
		t.Fatalf("Parse rendered config: %v\n%s", err, raw)
	}
	if !reflect.DeepEqual(*parsed, cfg) {
		t.Fatalf("round trip mismatch\nparsed: %#v\nwant:   %#v\nyaml:\n%s", *parsed, cfg, raw)
	}
}

func TestNewDraftUsesIsolatedAbsoluteRuntimePaths(t *testing.T) {
	stateRoot := t.TempDir()
	draft := setup.NewDraft("product-a", "/opt/clipse", stateRoot)

	if draft.Config.AgentBackend.Type != "daytona" {
		t.Errorf("backend = %q, want recommended daytona", draft.Config.AgentBackend.Type)
	}
	if got, want := draft.Config.BoardDir, filepath.Join(stateRoot, "product-a"); got != want {
		t.Errorf("BoardDir = %q, want %q", got, want)
	}
	if got, want := draft.Config.CheckpointsDir, filepath.Join(stateRoot, "product-a", "checkpoints"); got != want {
		t.Errorf("CheckpointsDir = %q, want %q", got, want)
	}
	wantWorker := []string{"uv", "--project", "/opt/clipse/agent", "run", "clipse-worker"}
	if !reflect.DeepEqual(draft.Config.Worker.Command, wantWorker) {
		t.Errorf("worker = %#v, want %#v", draft.Config.Worker.Command, wantWorker)
	}
}
