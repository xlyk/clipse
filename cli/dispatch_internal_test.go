package cli

import (
	"slices"
	"testing"
)

// White-box (same-package) unit tests for the pure CLI-flag-vs-config
// resolution helpers runDispatch uses to build the real Dispatcher — see
// dispatch.go. Kept separate from dispatch_test.go (package cli_test) so
// these can exercise the unexported resolve* functions directly, mirroring
// the existing white-box precedent in this repo (e.g.
// dispatcher/singleton_test.go, internal/board/board_test.go).

func TestResolveBoardDir(t *testing.T) {
	tests := []struct {
		name      string
		flagValue string
		cfgValue  string
		want      string
	}{
		{
			name:      "explicit --board flag overrides config board_dir",
			flagValue: "/flag/board",
			cfgValue:  "/cfg/board",
			want:      "/flag/board",
		},
		{
			name:      "empty flag (not passed) falls back to config board_dir",
			flagValue: "",
			cfgValue:  "/cfg/board",
			want:      "/cfg/board",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBoardDir(tt.flagValue, tt.cfgValue); got != tt.want {
				t.Errorf("resolveBoardDir(%q, %q) = %q, want %q", tt.flagValue, tt.cfgValue, got, tt.want)
			}
		})
	}
}

func TestResolveWorkerCommand(t *testing.T) {
	tests := []struct {
		name       string
		flagValue  string
		cfgCommand []string
		want       []string
	}{
		{
			name:       "explicit --worker flag overrides config as a single-element command",
			flagValue:  "/path/to/testworker",
			cfgCommand: []string{"uv", "--project", "/abs/agent", "run", "clipse-worker"},
			want:       []string{"/path/to/testworker"},
		},
		{
			name:       "empty flag (not passed) falls back to config worker.command",
			flagValue:  "",
			cfgCommand: []string{"uv", "--project", "/abs/agent", "run", "clipse-worker"},
			want:       []string{"uv", "--project", "/abs/agent", "run", "clipse-worker"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveWorkerCommand(tt.flagValue, tt.cfgCommand)
			if !slices.Equal(got, tt.want) {
				t.Errorf("resolveWorkerCommand(%q, %v) = %v, want %v", tt.flagValue, tt.cfgCommand, got, tt.want)
			}
		})
	}
}
