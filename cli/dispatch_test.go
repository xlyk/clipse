package cli_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xlyk/clipse/cli"
	"github.com/xlyk/clipse/dispatcher"
)

// minimalConfigYAML is a valid clipse.yaml with every required field set,
// used so tests that need config.Load to succeed don't have to depend on
// configs/clipse.example.yaml's location relative to the test binary.
const minimalConfigYAML = `
repo:
  remote: "https://example.invalid/repo.git"
  path: "/tmp/clipse-test-repo"
  base_branch: "main"
team_key: "CLI"
team_id: "8b5b3301-8da3-4933-9b07-9efc027bc09d"
worker:
  command:
    - clipse-worker
`

func TestDispatchCmd_Help(t *testing.T) {
	cmd := cli.NewRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"dispatch", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got := buf.String()
	for _, want := range []string{"--config", "--board", "--worker"} {
		if !strings.Contains(got, want) {
			t.Errorf("help output missing flag %q, got:\n%s", want, got)
		}
	}
}

// TestDispatchCmd_RefusesWhenLockHeld asserts that a second `clipse
// dispatch` invocation against a --board whose lockfile another dispatcher
// already holds fails fast with ErrAlreadyRunning, rather than trying to
// proceed into store.Open/Run. A valid --config is supplied so config.Load
// succeeds and the invocation actually reaches the lock check.
func TestDispatchCmd_RefusesWhenLockHeld(t *testing.T) {
	boardDir := t.TempDir()
	lockPath := filepath.Join(boardDir, "clipse.lock")
	configPath := filepath.Join(boardDir, "clipse.yaml")
	if err := os.WriteFile(configPath, []byte(minimalConfigYAML), 0o644); err != nil {
		t.Fatalf("writing test config: unexpected error: %v", err)
	}

	release, err := dispatcher.AcquireSingleton(lockPath)
	if err != nil {
		t.Fatalf("pre-acquiring lock: unexpected error: %v", err)
	}
	defer release()

	cmd := cli.NewRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"dispatch", "--board", boardDir, "--config", configPath})

	err = cmd.Execute()
	if !errors.Is(err, dispatcher.ErrAlreadyRunning) {
		t.Fatalf("Execute() error = %v, want ErrAlreadyRunning", err)
	}
}
