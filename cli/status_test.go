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

// seedStatusStore opens a Store backed by a fresh SQLite file in a temp dir
// and seeds it with a handful of issues across assorted board_status values
// plus a couple of runs, so RenderStatus has real per-status counts and
// per-issue run state to render.
func seedStatusStore(t *testing.T) store.Snapshot {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "clipse.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: unexpected error: %v", err)
		}
	})

	ctx := context.Background()
	issues := []store.Issue{
		{ID: "issue-1", Identifier: "CLP-1", LaneLabel: "agent:coder", BoardStatus: "running", CreatedAt: 100, UpdatedAt: 100, LastSeen: 100},
		{ID: "issue-2", Identifier: "CLP-2", LaneLabel: "agent:coder", BoardStatus: "ready", CreatedAt: 100, UpdatedAt: 100, LastSeen: 100},
		{ID: "issue-3", Identifier: "CLP-3", LaneLabel: "agent:reviewer", BoardStatus: "blocked", CreatedAt: 100, UpdatedAt: 100, LastSeen: 100},
		{ID: "issue-4", Identifier: "CLP-4", LaneLabel: "agent:reviewer", BoardStatus: "review", CreatedAt: 100, UpdatedAt: 100, LastSeen: 100},
	}
	for _, issue := range issues {
		if err := s.UpsertIssue(ctx, issue); err != nil {
			t.Fatalf("UpsertIssue(%s): unexpected error: %v", issue.ID, err)
		}
	}

	// issue-1 has a running run at turn 2.
	if err := s.InsertRun(ctx, store.Run{
		RunID:     "run-1",
		IssueID:   "issue-1",
		Lane:      "coder",
		Status:    "running",
		StartedAt: 100,
		Attempt:   1,
		TurnCount: 2,
		ThreadID:  "thread-1",
	}); err != nil {
		t.Fatalf("InsertRun(run-1): unexpected error: %v", err)
	}

	// issue-4 has a closed/done run.
	if err := s.InsertRun(ctx, store.Run{
		RunID:     "run-2",
		IssueID:   "issue-4",
		Lane:      "reviewer",
		Status:    "running",
		StartedAt: 100,
		Attempt:   1,
		TurnCount: 1,
		ThreadID:  "thread-2",
	}); err != nil {
		t.Fatalf("InsertRun(run-2): unexpected error: %v", err)
	}
	if err := s.CloseRun(ctx, "run-2", "done", `{"outcome":"done"}`, "", 10, 20); err != nil {
		t.Fatalf("CloseRun(run-2): unexpected error: %v", err)
	}

	snap, err := s.ReadSnapshot(ctx)
	if err != nil {
		t.Fatalf("ReadSnapshot: unexpected error: %v", err)
	}
	return snap
}

func TestRenderStatus_SummaryAndPerIssueTable(t *testing.T) {
	snap := seedStatusStore(t)

	var buf bytes.Buffer
	if err := cli.RenderStatus(&buf, snap); err != nil {
		t.Fatalf("RenderStatus: unexpected error: %v", err)
	}
	got := buf.String()

	// Per-status summary must surface counts for the statuses present.
	for _, want := range []string{"running", "ready", "blocked", "review"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing status %q, got:\n%s", want, got)
		}
	}

	// Per-issue rows: identifiers and statuses must appear.
	for _, want := range []string{"CLP-1", "CLP-2", "CLP-3", "CLP-4"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing identifier %q, got:\n%s", want, got)
		}
	}

	// issue-1's running run with turn 2 should show up somewhere.
	if !strings.Contains(got, "2") {
		t.Errorf("output missing turn count for issue-1's run, got:\n%s", got)
	}

	// issue-2 (ready) has no run at all: expect a placeholder rather than a
	// blank/garbled cell.
	lines := strings.Split(got, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "CLP-2") {
			found = true
			if !strings.Contains(line, "-") {
				t.Errorf("issue-2 row missing '-' placeholder for no-run, got line: %q", line)
			}
		}
	}
	if !found {
		t.Fatalf("no output line contained CLP-2")
	}
}

func TestRenderStatus_EmptySnapshot(t *testing.T) {
	var buf bytes.Buffer
	err := cli.RenderStatus(&buf, store.Snapshot{CountsByStatus: map[string]int{}})
	if err != nil {
		t.Fatalf("RenderStatus: unexpected error: %v", err)
	}
	if buf.Len() == 0 {
		t.Errorf("RenderStatus on empty snapshot: got empty output, want at least a header/summary")
	}
}

func TestStatusCmd_Help(t *testing.T) {
	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "--board") {
		t.Errorf("help output missing --board flag, got:\n%s", got)
	}
}

// TestStatusCmd_NoBoardFound asserts that pointing --board at a directory
// with no clipse.db yields a friendly, non-zero-exit error rather than a
// raw sqlite/driver error.
func TestStatusCmd_NoBoardFound(t *testing.T) {
	boardDir := t.TempDir()

	cmd := cli.NewRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"status", "--board", boardDir})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute() error = nil, want error for missing board db")
	}
	if !strings.Contains(err.Error(), boardDir) {
		t.Errorf("error %v does not mention board path %q", err, boardDir)
	}
}

// TestStatusCmd_RendersSeededBoard asserts the full wiring: opening a real
// store at --board, reading its snapshot, and writing the rendered table to
// stdout.
func TestStatusCmd_RendersSeededBoard(t *testing.T) {
	boardDir := t.TempDir()
	dbPath := filepath.Join(boardDir, "clipse.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: unexpected error: %v", err)
	}
	ctx := context.Background()
	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", BoardStatus: "ready", CreatedAt: 1, UpdatedAt: 1, LastSeen: 1}); err != nil {
		t.Fatalf("UpsertIssue: unexpected error: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}

	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--board", boardDir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "CLP-1") {
		t.Errorf("stdout missing CLP-1, got:\n%s", got)
	}
	if !strings.Contains(got, "ready") {
		t.Errorf("stdout missing status 'ready', got:\n%s", got)
	}
}

// TestStatusCmd_DefaultBoardFlag asserts --board defaults to ./.clipse,
// mirroring dispatch.go's default.
func TestStatusCmd_DefaultBoardFlag(t *testing.T) {
	cmd := cli.NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"status", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out.String(), "./.clipse") {
		t.Errorf("help output missing default board path './.clipse', got:\n%s", out.String())
	}
}
