package tui

import (
	"database/sql"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)

// TestKindLabel asserts raw event kinds map to short, fixed-width-friendly
// labels so the activity feed never truncates a kind mid-word, and that the
// P3 translations name what actually happened ("stale_release" is a routine
// claim-expiry requeue, not an alarming "stale").
func TestKindLabel(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"rework_cap_exceeded", "rework cap"},
		{"request_changes", "changes req"},
		{"open_review", "review"},
		{"claimed", "claimed"},
		{"auto_merged", "merged"},
		{"merge", "merged"},
		{"done", "complete"},
		{"complete", "complete"},
		{"promoted", "promoted"},
		{"blocked", "blocked"},
		{"comment_block", "blocked"},
		{"stale_release", "requeued"},
		{"retry_scheduled", "retry"},
		{"orphaned", "orphaned"},
		{"orphan_requeue", "orphaned"},
		{"queued", "queued"}, // unknown, short: passed through
	}
	for _, tt := range tests {
		if got := kindLabel(tt.kind); got != tt.want {
			t.Errorf("kindLabel(%q) = %q, want %q", tt.kind, got, tt.want)
		}
		if got := kindLabel(tt.kind); len([]rune(got)) > 11 {
			t.Errorf("kindLabel(%q) = %q is wider than the 11-col kind column", tt.kind, got)
		}
	}
}

// TestClassifyEvent asserts the two-class split (P3): verdicts are the
// board-moving outcomes (the only loud feed lines), mechanics are kernel
// bookkeeping (always dim), and open_review / unknown kinds keep the neutral
// middle weight.
func TestClassifyEvent(t *testing.T) {
	verdicts := []string{"merge", "auto_merged", "done", "complete", "blocked", "comment_block", "request_changes", "rework_cap_exceeded"}
	for _, kind := range verdicts {
		if got := classifyEvent(kind); got != classVerdict {
			t.Errorf("classifyEvent(%q) = %v, want classVerdict", kind, got)
		}
	}
	mechanics := []string{"claimed", "promoted", "adopted", "stale_release", "retry_scheduled", "orphaned", "orphan_requeue", "respawn"}
	for _, kind := range mechanics {
		if got := classifyEvent(kind); got != classMechanic {
			t.Errorf("classifyEvent(%q) = %v, want classMechanic", kind, got)
		}
	}
	for _, kind := range []string{"open_review", "somethingelse"} {
		if got := classifyEvent(kind); got != classNeutral {
			t.Errorf("classifyEvent(%q) = %v, want classNeutral", kind, got)
		}
	}
}

// TestCleanActivityDetail asserts the feed detail is de-noised and
// translated (P3): "claimed" collapses to a short run id; a stale release
// reads as the routine claim-expiry requeue it is (target column named, no
// 32-char token, no "merging -> merging" arrow); a scheduled retry names
// attempt/cap and the reason; long hex ids everywhere else are shortened;
// multi-line details flatten to one line.
func TestCleanActivityDetail(t *testing.T) {
	tests := []struct {
		name, kind, detail, want string
	}{
		{"claimed strips prefix + shortens uuid", "claimed", "claimed by run 8494b1cc1690b9e368059c9db9d6717c", "run 8494b1cc"},
		{"claimed short id kept", "claimed", "claimed by run abc123", "run abc123"},
		{
			"stale release translated",
			"stale_release",
			"released stale claim 5ae111436e2f14e8781f2404f97ccb90 (column merging -> merging)",
			"claim expired — requeued in merging",
		},
		{"stale release unparseable falls back", "stale_release", "released stale claim garbled", "claim expired — requeued"},
		{
			"retry scheduled translated",
			"retry_scheduled",
			"auto-retry 1/2 after transient failure: worker crashed",
			"transient failure — retry 1/2: worker crashed",
		},
		{
			"retry reason hex ids shortened",
			"retry_scheduled",
			"auto-retry 2/2 after transient failure: worker crashed at commit 8494b1cc1690b9e368059c9db9d6717c",
			"transient failure — retry 2/2: worker crashed at commit 8494b1cc",
		},
		{
			"hex ids shortened in other details",
			"orphan_requeue",
			"requeued orphan run 8494b1cc1690b9e368059c9db9d6717c",
			"requeued orphan run 8494b1cc",
		},
		{"multiline flattened", "open_review", "line one\nline two", "line one line two"},
		{"whitespace collapsed", "request_changes", "  a   b\t c ", "a b c"},
	}
	for _, tt := range tests {
		if got := cleanActivityDetail(tt.kind, tt.detail); got != tt.want {
			t.Errorf("%s: cleanActivityDetail(%q, %q) = %q, want %q", tt.name, tt.kind, tt.detail, got, tt.want)
		}
	}
}

// TestActivityLines_LaneDot asserts each feed row carries a lane marker
// resolved through the issue's runs (run_id → lane): a resolvable run gets
// the ● dot, an event with no run (or an unknown run) gets the dim ·
// placeholder. Colors are stripped in a non-TTY test run, so the glyphs are
// asserted as plain text.
func TestActivityLines_LaneDot(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{
				Issue: store.Issue{ID: "i-1", Identifier: "CLI-1", LaneLabel: "coder", BoardStatus: "review"},
				Runs: []store.Run{
					{RunID: "run-rev", IssueID: "i-1", Lane: "reviewer", Status: "running"},
				},
			},
		},
		RecentEvents: []store.Event{
			{
				ID: 2, Ts: 200,
				IssueID: sql.NullString{String: "i-1", Valid: true},
				RunID:   sql.NullString{String: "run-rev", Valid: true},
				Kind:    "request_changes", Detail: "needs a fix",
			},
			{
				ID: 1, Ts: 100,
				IssueID: sql.NullString{String: "i-1", Valid: true},
				Kind:    "promoted", Detail: "deps satisfied",
			},
		},
	}

	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	lines := m.activityLines(100)
	if len(lines) != 2 {
		t.Fatalf("activityLines returned %d lines, want 2:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "●") {
		t.Errorf("event with resolvable run lane missing ● dot: %q", lines[0])
	}
	if !strings.Contains(lines[1], "·") {
		t.Errorf("event without a run missing · placeholder: %q", lines[1])
	}
}
