package cli_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xlyk/clipse/cli"
	"github.com/xlyk/clipse/internal/store"
)

func newControlBoard(t *testing.T) (string, *store.Store) {
	t.Helper()
	boardDir := t.TempDir()
	s, err := store.Open(filepath.Join(boardDir, "clipse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return boardDir, s
}

func executeControl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestDispatchPauseCommandCommitsBoardScopedBarrier(t *testing.T) {
	boardDir, s := newControlBoard(t)
	out, err := executeControl(t, "dispatch", "pause", "--board", boardDir)
	if err != nil {
		t.Fatal(err)
	}
	control, err := s.ReadDispatcherControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingPaused || control.RequestID == "" {
		t.Fatalf("control = %+v, want requested pause", control)
	}
	abs, _ := filepath.Abs(boardDir)
	if !strings.Contains(out, abs) || !strings.Contains(out, "paused") {
		t.Fatalf("pause output missing board/mode evidence:\n%s", out)
	}
}

func TestDispatchDrainWithoutDaemonLeavesBoardPausedAndReportsNoTarget(t *testing.T) {
	boardDir, s := newControlBoard(t)
	_, err := executeControl(t, "dispatch", "drain", "--board", boardDir)
	if err == nil || !strings.Contains(err.Error(), "no active dispatcher") {
		t.Fatalf("drain error = %v, want no active dispatcher diagnostic", err)
	}
	control, readErr := s.ReadDispatcherControl(context.Background())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if control.DesiredMode != store.SchedulingPaused || control.DrainTargetInstanceID != "" {
		t.Fatalf("control after untargeted drain = %+v", control)
	}
}

func TestDispatchResumeRequiresExplicitDrainCancellation(t *testing.T) {
	boardDir, s := newControlBoard(t)
	ctx := context.Background()
	if _, err := s.RegisterDispatcher(ctx, "instance-a", 1234, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestDrain(ctx, "drain-1", 20, false); err != nil {
		t.Fatal(err)
	}
	if _, err := executeControl(t, "dispatch", "resume", "--board", boardDir); err == nil || !strings.Contains(err.Error(), "--cancel-drain") {
		t.Fatalf("plain resume error = %v, want --cancel-drain diagnostic", err)
	}
	if _, err := executeControl(t, "dispatch", "resume", "--board", boardDir, "--cancel-drain"); err != nil {
		t.Fatal(err)
	}
	control, err := s.ReadDispatcherControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingRunning || control.DrainTargetInstanceID != "" {
		t.Fatalf("control after cancel/resume = %+v", control)
	}
}

func TestDispatchControlStatusMakesRestartSafetyExplicit(t *testing.T) {
	boardDir, s := newControlBoard(t)
	if err := s.RequestPause(context.Background(), "pause-1", 10); err != nil {
		t.Fatal(err)
	}
	out, err := executeControl(t, "dispatch", "control-status", "--board", boardDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"desired mode", "paused",
		"active runs", "0",
		"pending outbox", "0",
		"pending cleanup", "0",
		"safe to restart", "yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("control-status missing %q:\n%s", want, out)
		}
	}
}

func TestDispatchControlRequiresExistingBoard(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, err := executeControl(t, "dispatch", "pause", "--board", missing)
	if err == nil || !strings.Contains(err.Error(), "no clipse board") {
		t.Fatalf("pause missing-board error = %v", err)
	}
}
