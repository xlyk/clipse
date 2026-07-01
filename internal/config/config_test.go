package config_test

import (
	"os"
	"path/filepath"
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

func TestLoad_ValidFullConfig(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
poll_interval_s: 45
caps:
  global: 10
  per_lane:
    coder: 5
    reviewer: 3
    git_operator: 2
    scribe: 2
turn_cap: 7
max_runtime_s: 1800
lane_label_prefix: "lane:"
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
	if cfg.Caps.PerLane.Scribe != 2 {
		t.Errorf("Caps.PerLane.Scribe = %d, want 2", cfg.Caps.PerLane.Scribe)
	}
	if cfg.TurnCap != 7 {
		t.Errorf("TurnCap = %d, want 7", cfg.TurnCap)
	}
	if cfg.MaxRuntimeS != 1800 {
		t.Errorf("MaxRuntimeS = %d, want 1800", cfg.MaxRuntimeS)
	}
	if cfg.LaneLabelPrefix != "lane:" {
		t.Errorf("LaneLabelPrefix = %q, want %q", cfg.LaneLabelPrefix, "lane:")
	}
}

func TestLoad_MinimalConfigGetsDefaults(t *testing.T) {
	path := writeYAML(t, `
repo:
  remote: "https://github.com/yourorg/yourrepo.git"
  path: "/home/you/code/yourrepo"
  base_branch: "main"
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
	if cfg.Caps.PerLane.Scribe != 1 {
		t.Errorf("Caps.PerLane.Scribe = %d, want default 1", cfg.Caps.PerLane.Scribe)
	}
	if cfg.TurnCap != 5 {
		t.Errorf("TurnCap = %d, want default 5", cfg.TurnCap)
	}
	if cfg.MaxRuntimeS != 3600 {
		t.Errorf("MaxRuntimeS = %d, want default 3600", cfg.MaxRuntimeS)
	}
	if cfg.LaneLabelPrefix != "agent:" {
		t.Errorf("LaneLabelPrefix = %q, want default %q", cfg.LaneLabelPrefix, "agent:")
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
