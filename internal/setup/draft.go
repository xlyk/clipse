// Package setup contains the model-neutral core used by the interactive
// configuration wizard: draft defaults, deterministic YAML rendering,
// readiness checks, and safe file writes.
package setup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xlyk/clipse/internal/config"
)

// Draft is the wizard's durable, non-secret state. Credential values never
// belong here; probes read them from the current process environment.
type Draft struct {
	Instance string
	Config   config.Config
}

// DefaultStateRoot returns the platform-appropriate host-local state root
// used for named Clipse instances.
func DefaultStateRoot(home string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "clipse")
	}
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "clipse")
	}
	return filepath.Join(home, ".local", "state", "clipse")
}

// NewDraft returns the recommended Daytona-first starting point for one
// isolated instance. stateRoot is a host-local absolute directory beneath
// which the instance gets its own board and checkpoint state.
func NewDraft(instance, clipseRoot, stateRoot string) Draft {
	instance = strings.TrimSpace(instance)
	if instance == "" {
		instance = "default"
	}
	cfg := config.Defaults()
	cfg.AgentBackend.Type = "daytona"
	cfg.Worker.Command = []string{
		"uv", "--project", filepath.Join(clipseRoot, "agent"), "run", "clipse-worker",
	}
	cfg.BoardDir = filepath.Join(stateRoot, instance)
	cfg.CheckpointsDir = filepath.Join(cfg.BoardDir, "checkpoints")
	return Draft{Instance: instance, Config: cfg}
}
