package tui

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// TestParseTranscriptLine asserts the lenient JSONL decode: well-formed
// events parse their typed fields; a malformed line degrades to a raw-text
// event (Event == "") instead of an error — a garbled transcript line must
// never break the follow view.
func TestParseTranscriptLine(t *testing.T) {
	ev := parseTranscriptLine(`{"lane":"reviewer","run_id":"r1","model":"anthropic:claude-opus-4-6","event":"turn_start","task_text":"review PR 44","ts":100.5}`)
	if ev.Event != "turn_start" || ev.Lane != "reviewer" || ev.RunID != "r1" || ev.TaskText != "review PR 44" || ev.Ts != 100.5 {
		t.Errorf("turn_start parse = %+v", ev)
	}

	ev = parseTranscriptLine(`{"lane":"coder","event":"tool_call","name":"shell","args":{"command":"go test ./..."},"ts":101.0}`)
	if ev.Event != "tool_call" || ev.Name != "shell" {
		t.Errorf("tool_call parse = %+v", ev)
	}
	if cmd, _ := ev.Args["command"].(string); cmd != "go test ./..." {
		t.Errorf("tool_call args.command = %v, want %q", ev.Args["command"], "go test ./...")
	}

	ev = parseTranscriptLine(`{"lane":"coder","event":"turn_end","outcome_hint":"completed","tokens_in":489800,"tokens_out":4800,"ts":120.0}`)
	if ev.Event != "turn_end" || ev.OutcomeHint != "completed" || ev.TokensIn != 489800 || ev.TokensOut != 4800 {
		t.Errorf("turn_end parse = %+v", ev)
	}

	ev = parseTranscriptLine(`{not json at all`)
	if ev.Event != "" {
		t.Errorf("malformed line Event = %q, want \"\" (raw fallback)", ev.Event)
	}
	if ev.raw != `{not json at all` {
		t.Errorf("malformed line raw = %q, want the original line kept", ev.raw)
	}
}

// TestFollowState_ApplyChunk_PartialLineCarry asserts only complete lines
// are parsed: a poll that ends mid-line carries the fragment to the next
// applyChunk, so a JSON object split across two reads still parses whole.
func TestFollowState_ApplyChunk_PartialLineCarry(t *testing.T) {
	f := followState{source: followTranscript}

	f.applyChunk([]byte(`{"event":"turn_start","lane":"coder","ts":1}`+"\n"+`{"event":"assist`), 60, false)
	if len(f.events) != 1 {
		t.Fatalf("events after partial chunk = %d, want 1 (fragment must not parse yet)", len(f.events))
	}
	if f.events[0].Event != "turn_start" {
		t.Errorf("events[0].Event = %q, want turn_start", f.events[0].Event)
	}

	f.applyChunk([]byte(`ant","text":"hi","ts":2}`+"\n"), 90, false)
	if len(f.events) != 2 {
		t.Fatalf("events after completing the line = %d, want 2", len(f.events))
	}
	if f.events[1].Event != "assistant" || f.events[1].Text != "hi" {
		t.Errorf("events[1] = %+v, want the rejoined assistant event", f.events[1])
	}
	if f.offset != 90 {
		t.Errorf("offset = %d, want 90", f.offset)
	}
}

// TestFollowState_ApplyChunk_ResetClearsBuffers asserts the O_TRUNC respawn
// path: reset=true drops accumulated events/lines and the partial fragment
// before folding in the fresh read.
func TestFollowState_ApplyChunk_ResetClearsBuffers(t *testing.T) {
	f := followState{source: followRaw}
	f.applyChunk([]byte("old line 1\nold li"), 17, false)
	if len(f.rawLines) != 1 {
		t.Fatalf("setup: rawLines = %d, want 1", len(f.rawLines))
	}

	f.applyChunk([]byte("fresh line\n"), 11, true)
	if len(f.rawLines) != 1 || f.rawLines[0] != "fresh line" {
		t.Errorf("rawLines after reset = %#v, want just [\"fresh line\"]", f.rawLines)
	}
	if f.offset != 11 {
		t.Errorf("offset after reset = %d, want 11", f.offset)
	}
	if len(f.partial) != 0 {
		t.Errorf("partial after reset = %q, want empty", f.partial)
	}
}

// TestFollowState_Lane asserts lane() reports the most recent turn_start's
// lane (the agent currently — or last — driving the issue), bare-normalized.
func TestFollowState_Lane(t *testing.T) {
	f := followState{source: followTranscript}
	if got := f.lane(); got != "" {
		t.Errorf("lane() on empty state = %q, want \"\"", got)
	}
	f.events = []transcriptEvent{
		{Event: "turn_start", Lane: "coder"},
		{Event: "turn_end"},
		{Event: "turn_start", Lane: "agent:reviewer"},
		{Event: "assistant", Text: "hi"},
	}
	if got := f.lane(); got != "reviewer" {
		t.Errorf("lane() = %q, want %q (latest turn_start, bare)", got, "reviewer")
	}
}

// TestRenderTranscriptLines asserts the semantic rendering (colors are
// stripped in a non-TTY test run, so plain substrings are asserted):
// turn_start → a lane-named header with a bare model name; a shell tool_call →
// a "$ <cmd>" line that gains a "✓ · <dur>s" badge once its result arrives; a
// read_file → a "read <relpath>" line (resolved arg, not a bare tool name);
// assistant text → wrapped; turn_end → an outcome + token rule; malformed →
// dim raw text.
func TestRenderTranscriptLines(t *testing.T) {
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "reviewer", RunID: "8494b1cc1690b9e368059c9db9d6717c", Model: "anthropic:claude-opus-4-6", Ts: 100},
		{Event: "tool_call", Name: "execute", Args: map[string]any{"command": "go test ./..."}, Ts: 101},
		{Event: "tool_result", Name: "execute", Status: "success", Content: "ok\n[Command succeeded with exit code 0]", Ts: 103.1},
		{Event: "assistant", Text: "All tests pass. Verifying the constraint next.", Ts: 104},
		{Event: "tool_call", Name: "read_file", Args: map[string]any{"file_path": "/b/worktrees/slug/pkg/x.go"}, Ts: 105},
		{Event: "turn_end", OutcomeHint: "completed", TokensIn: 489800, TokensOut: 4800, Ts: 120},
		{raw: "not json"},
	}

	out := strings.Join(renderTranscriptLines(events, 80), "\n")

	if !strings.Contains(out, "reviewer") {
		t.Errorf("output missing the turn_start lane header:\n%s", out)
	}
	// Model rendered bare (provider prefix stripped).
	if !strings.Contains(out, "claude-opus-4-6") || strings.Contains(out, "anthropic:claude-opus-4-6") {
		t.Errorf("turn_start model not rendered bare (want provider prefix stripped):\n%s", out)
	}
	if !strings.Contains(out, "$ go test ./...") {
		t.Errorf("output missing the shell tool_call one-liner:\n%s", out)
	}
	if !strings.Contains(out, "✓") {
		t.Errorf("output missing the matched result ✓ badge:\n%s", out)
	}
	if !strings.Contains(out, "All tests pass.") {
		t.Errorf("output missing the assistant text:\n%s", out)
	}
	// read_file's arg resolves to a repo-relative path, not the raw tool name.
	if !strings.Contains(out, "read") || !strings.Contains(out, "pkg/x.go") {
		t.Errorf("output missing the resolved read_file arg:\n%s", out)
	}
	// The still-unanswered read_file call renders with a pending marker.
	if !strings.Contains(out, "…") {
		t.Errorf("output missing the pending marker for the unanswered call:\n%s", out)
	}
	if !strings.Contains(out, "↓489.8k") || !strings.Contains(out, "↑4.8k") {
		t.Errorf("output missing turn_end tokens:\n%s", out)
	}
	if !strings.Contains(out, "not json") {
		t.Errorf("output missing the malformed line's raw fallback:\n%s", out)
	}
}

// TestRenderTranscriptLines_FailureSurfaced asserts the failure-biased result
// rendering: an execute whose tool wrapper reported status="success" but whose
// COMMAND exited non-zero is shown as a failure (✗ exit N) with the error tail
// surfaced inline — the single most important correctness fix in the redesign,
// since the flat renderer labeled these "ok".
func TestRenderTranscriptLines_FailureSurfaced(t *testing.T) {
	content := "[stderr] Traceback (most recent call last):\n" +
		"[stderr]   File \"<stdin>\", line 1, in <module>\n" +
		"[stderr] ModuleNotFoundError: No module named 'estimator_v2'\n\n" +
		"Exit code: 1\n[Command failed with exit code 1]"
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "coder", Ts: 100},
		{Event: "tool_call", Name: "execute", Args: map[string]any{"command": "python - <<'PY'\nimport estimator_v2\nPY"}, Ts: 101},
		{Event: "tool_result", Name: "execute", Status: "success", Content: content, Ts: 102},
	}
	out := strings.Join(renderTranscriptLines(events, 90), "\n")

	if !strings.Contains(out, "✗ exit 1") {
		t.Errorf("failed command not surfaced as ✗ exit 1 (status was a misleading \"success\"):\n%s", out)
	}
	if !strings.Contains(out, "ModuleNotFoundError: No module named 'estimator_v2'") {
		t.Errorf("error tail not surfaced inline:\n%s", out)
	}
	// The command's first line shows; the heredoc body must not flood the view.
	if !strings.Contains(out, "$ python - <<'PY'") {
		t.Errorf("command first line missing:\n%s", out)
	}
}

// TestRenderTranscriptLines_EditDiff asserts edit_file renders its
// old_string/new_string as a red/green line diff, so the actual change is
// visible instead of a bare "edit_file → ok".
func TestRenderTranscriptLines_EditDiff(t *testing.T) {
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "coder", Ts: 100},
		{Event: "tool_call", Name: "edit_file", Args: map[string]any{
			"file_path":  "/b/worktrees/slug/apps/x/contingency.py",
			"old_string": "flat known-risk contingency\nsecond old line",
			"new_string": "deduped adverse fork exposure\nsecond new line",
		}, Ts: 101},
		{Event: "tool_result", Name: "edit_file", Status: "success", Content: "Successfully replaced 1 instance(s)", Ts: 102},
	}
	out := strings.Join(renderTranscriptLines(events, 90), "\n")

	if !strings.Contains(out, "edit") || !strings.Contains(out, "apps/x/contingency.py") {
		t.Errorf("edit_file header missing resolved path:\n%s", out)
	}
	if !strings.Contains(out, "- flat known-risk contingency") {
		t.Errorf("edit diff missing the removed line:\n%s", out)
	}
	if !strings.Contains(out, "+ deduped adverse fork exposure") {
		t.Errorf("edit diff missing the added line:\n%s", out)
	}
}

// TestRenderTranscriptLines_GrepMatches asserts grep resolves its pattern +
// path and reports a match count from the result content.
func TestRenderTranscriptLines_GrepMatches(t *testing.T) {
	content := "apps/x/contingency.py:\n  277: def fork_contingency(forks):\n  512: fork_contingency(open)\n  640:     return fork_contingency(x)"
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "coder", Ts: 100},
		{Event: "tool_call", Name: "grep", Args: map[string]any{"pattern": "fork_contingency(", "path": "/b/worktrees/slug/apps/x"}, Ts: 101},
		{Event: "tool_result", Name: "grep", Status: "success", Content: content, Ts: 102},
	}
	out := strings.Join(renderTranscriptLines(events, 90), "\n")

	if !strings.Contains(out, "grep") || !strings.Contains(out, "fork_contingency(") {
		t.Errorf("grep pattern not resolved:\n%s", out)
	}
	if !strings.Contains(out, "3 matches") {
		t.Errorf("grep match count not reported:\n%s", out)
	}
}

// TestTranscriptRenderHelpers locks the pure helpers the failure-biased
// rendering hinges on: worktree-relative paths, real exit-code extraction, and
// grep match counting.
func TestTranscriptRenderHelpers(t *testing.T) {
	// relPath strips the <board>/worktrees/<slug>/ prefix.
	if got := relPath("/x/board/worktrees/kyle-spa-1-slug/apps/a.py"); got != "apps/a.py" {
		t.Errorf("relPath = %q, want apps/a.py", got)
	}
	if got := relPath("/x/board/worktrees/kyle-spa-1-slug"); got != "." {
		t.Errorf("relPath of a bare worktree root = %q, want .", got)
	}
	if got := relPath("/etc/passwd"); got != "/etc/passwd" {
		t.Errorf("relPath of a non-worktree path = %q, want it unchanged", got)
	}

	// shortPath middle-elides but keeps the leading dir and the filename.
	long := "/x/board/worktrees/slug/apps/estimator/src/estimator/contingency.py"
	got := shortPath(long, 30)
	if len([]rune(got)) > 30 {
		t.Errorf("shortPath length = %d, want <= 30 (%q)", len([]rune(got)), got)
	}
	if !strings.HasPrefix(got, "apps") || !strings.HasSuffix(got, "contingency.py") || !strings.Contains(got, "…") {
		t.Errorf("shortPath = %q, want apps…contingency.py shape", got)
	}

	// execExitCode reads the real command result out of an execute's content.
	if code, ok := execExitCode("boom\nExit code: 1\n[Command failed with exit code 1]"); !ok || code != 1 {
		t.Errorf("execExitCode(fail) = %d,%v, want 1,true", code, ok)
	}
	if code, ok := execExitCode("out\n[Command succeeded with exit code 0]"); !ok || code != 0 {
		t.Errorf("execExitCode(ok) = %d,%v, want 0,true", code, ok)
	}
	if _, ok := execExitCode("just some tool output, no marker"); ok {
		t.Errorf("execExitCode with no marker returned ok=true, want false")
	}

	// resultFailed: a shell success status is overridden by a non-zero exit.
	if !resultFailed("success", "x\n[Command failed with exit code 2]") {
		t.Errorf("resultFailed did not override a misleading success status")
	}
	if resultFailed("success", "file contents") {
		t.Errorf("resultFailed flagged a plain success")
	}

	// grepMatchCount counts line-numbered hit lines.
	if n := grepMatchCount("f.py:\n  1: a\n  2: b\n  3: c"); n != 3 {
		t.Errorf("grepMatchCount = %d, want 3", n)
	}
}

// TestRenderTranscriptLines_NeverPanics fuzzes the renderer with degenerate
// inputs: nil events, zero width, events with every field empty.
func TestRenderTranscriptLines_NeverPanics(t *testing.T) {
	_ = renderTranscriptLines(nil, 0)
	_ = renderTranscriptLines([]transcriptEvent{{}, {Event: "tool_result"}, {Event: "turn_end"}}, 1)
	_ = renderRawLines(nil, 0)
	_ = renderRawLines([]string{"x"}, 1)
}

// TestMatchToolResults_TurnScoped asserts tool pairing never crosses a turn
// boundary: a turn that ends with an unresolved tool_call (crash, interrupt,
// ceiling abort — dac.py's _flush_pending emits the calls even when the
// ToolNode never ran) must not steal the next turn's same-named result. The
// open queues reset on every turn_start.
func TestMatchToolResults_TurnScoped(t *testing.T) {
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "coder", Ts: 100},
		{Event: "tool_call", Name: "shell", Args: map[string]any{"command": "go build ./..."}, Ts: 101},
		{Event: "turn_end", Error: "stream died", Ts: 102},
		{Event: "turn_start", Lane: "coder", Ts: 200},
		{Event: "tool_call", Name: "shell", Args: map[string]any{"command": "go test ./..."}, Ts: 201},
		{Event: "tool_result", Name: "shell", Status: "success", Content: "ok", Ts: 203.5},
	}

	outcomes, consumed := matchToolResults(events)
	if o, ok := outcomes[1]; ok {
		t.Errorf("turn 1's dead call (index 1) matched a result = %+v, want unresolved", o)
	}
	if o, ok := outcomes[4]; !ok || o.status != "success" {
		t.Errorf("turn 2's call (index 4) outcome = %+v ok=%v, want a success result", o, ok)
	}
	if !consumed[5] {
		t.Errorf("turn 2's result (index 5) not consumed, want consumed by its own turn's call")
	}

	out := strings.Join(renderTranscriptLines(events, 80), "\n")
	// The crashed turn's call renders unresolved (pending "…" badge, never a ✓).
	if !strings.Contains(out, "$ go build ./...") {
		t.Errorf("crashed turn's call missing:\n%s", out)
	}
	// The later turn's call pairs with its own result (✓ badge).
	if !strings.Contains(out, "$ go test ./...") || !strings.Contains(out, "✓") {
		t.Errorf("later turn's call must pair with its own result badge:\n%s", out)
	}
}

// TestPollFollowFile exercises the I/O command against real temp files: an
// incremental read from a stored offset, a missing file, and the O_TRUNC
// shrink → Reset + full re-read semantics the spec mandates.
func TestPollFollowFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "CLI-1.log")

	if err := os.WriteFile(path, []byte("line 1\nline 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	msg, ok := pollFollowFile(path, 0)().(followPollMsg)
	if !ok {
		t.Fatalf("pollFollowFile returned %T, want followPollMsg", msg)
	}
	if msg.Err != nil || msg.NotExist || msg.Reset {
		t.Fatalf("first poll = %+v, want clean read", msg)
	}
	if string(msg.Data) != "line 1\nline 2\n" || msg.Offset != 14 {
		t.Errorf("first poll Data = %q Offset = %d, want full file / 14", msg.Data, msg.Offset)
	}

	// Append; poll from the stored offset reads only the tail.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("line 3\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	msg = pollFollowFile(path, msg.Offset)().(followPollMsg)
	if string(msg.Data) != "line 3\n" || msg.Offset != 21 || msg.Reset {
		t.Errorf("incremental poll = %+v, want just the appended tail", msg)
	}

	// Shrink (a new spawn O_TRUNCs the stderr log): Reset + re-read from 0.
	if err := os.WriteFile(path, []byte("respawn\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	msg = pollFollowFile(path, msg.Offset)().(followPollMsg)
	if !msg.Reset {
		t.Errorf("poll after shrink Reset = false, want true")
	}
	if string(msg.Data) != "respawn\n" || msg.Offset != 8 {
		t.Errorf("poll after shrink = %+v, want the whole new file from 0", msg)
	}

	// Missing file: NotExist, no error.
	msg = pollFollowFile(filepath.Join(dir, "absent.log"), 0)().(followPollMsg)
	if !msg.NotExist || msg.Err != nil {
		t.Errorf("poll on absent file = %+v, want NotExist with nil Err", msg)
	}
}

// --- UI integration (Task 8) ---

// followSnap builds a snapshot with one live (claimed) row CLI-1, one parked
// review row CLI-2, and one todo row CLI-3.
func followSnap() store.Snapshot {
	claimed := sql.NullString{String: "claim-tok", Valid: true}
	return store.Snapshot{
		Issues: []store.IssueSnapshot{
			{
				Issue:     store.Issue{ID: "i-1", Identifier: "CLI-1", LaneLabel: "coder", BoardStatus: "running", ClaimLock: claimed},
				LatestRun: &store.Run{RunID: "r1", Lane: "coder", Status: "running", StartedAt: 100},
			},
			{Issue: store.Issue{ID: "i-2", Identifier: "CLI-2", LaneLabel: "coder", BoardStatus: "review"}},
			{Issue: store.Issue{ID: "i-3", Identifier: "CLI-3", LaneLabel: "coder", BoardStatus: "todo"}},
		},
	}
}

var keyF = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")}
var keyT = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")}

// TestFollowKey_OpensOnLiveRow asserts `f` on a live row enters follow mode
// and schedules the tail (a non-nil cmd: first poll + tick).
func TestFollowKey_OpensOnLiveRow(t *testing.T) {
	m := NewModel(WithLogsDir("/tmp/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})

	if got := m.Selected(); got != "CLI-1" {
		t.Fatalf("setup: Selected() = %q, want CLI-1", got)
	}
	m, cmd := m.Update(keyF)
	if got := m.ViewMode(); got != "follow" {
		t.Fatalf("after f ViewMode() = %q, want follow", got)
	}
	if cmd == nil {
		t.Error("after f cmd = nil, want the poll+tick batch")
	}
	if m.follow.ident != "CLI-1" || m.follow.source != followTranscript || !m.follow.pinned {
		t.Errorf("follow state = %+v, want CLI-1/transcript/pinned", m.follow)
	}
}

// TestFollowKey_GatedOnClaimOrTranscript asserts `f` is a no-op on a row
// with neither a live claim nor an on-disk transcript, and works on an
// unclaimed row once the refresh reports a transcript file for it.
func TestFollowKey_GatedOnClaimOrTranscript(t *testing.T) {
	m := NewModel(WithLogsDir("/tmp/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})

	// Move to CLI-3 (todo, unclaimed, no transcript): f must do nothing.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := m.Selected(); got != "CLI-3" {
		t.Fatalf("setup: Selected() = %q, want CLI-3", got)
	}
	m, _ = m.Update(keyF)
	if got := m.ViewMode(); got != "dashboard" {
		t.Errorf("f on unclaimed no-transcript row: ViewMode() = %q, want dashboard", got)
	}

	// A refresh reporting a transcript for CLI-2 unlocks f there.
	m, _ = m.Update(SnapshotMsg{Snap: followSnap(), TranscriptIdents: map[string]bool{"CLI-2": true}})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if got := m.Selected(); got != "CLI-2" {
		t.Fatalf("setup: Selected() = %q, want CLI-2", got)
	}
	m, _ = m.Update(keyF)
	if got := m.ViewMode(); got != "follow" {
		t.Errorf("f on transcript-bearing row: ViewMode() = %q, want follow", got)
	}
}

// TestFollowPoll_FoldsDataAndRenders asserts a followPollMsg's bytes reach
// the view: transcript events render semantically, and a stale poll (path no
// longer active after a toggle) is dropped.
func TestFollowPoll_FoldsDataAndRenders(t *testing.T) {
	m := NewModel(WithLogsDir("/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})
	m, _ = m.Update(keyF)

	data := []byte(`{"event":"turn_start","lane":"coder","run_id":"r1","model":"anthropic:claude-sonnet-4-6","ts":1}` + "\n" +
		`{"event":"assistant","text":"working on it","ts":2}` + "\n")
	m, _ = m.Update(followPollMsg{Path: m.followPath(), Data: data, Offset: int64(len(data))})

	if !strings.Contains(m.View(), "working on it") {
		t.Errorf("View() missing folded assistant text")
	}
	if m.follow.offset != int64(len(data)) {
		t.Errorf("offset = %d, want %d", m.follow.offset, len(data))
	}

	// A poll for a path that is no longer active must be dropped.
	before := len(m.follow.events)
	m, _ = m.Update(followPollMsg{Path: "/logs/OTHER.transcript.jsonl", Data: []byte(`{"event":"assistant","text":"stale","ts":3}` + "\n"), Offset: 999})
	if len(m.follow.events) != before || m.follow.offset != int64(len(data)) {
		t.Errorf("stale poll was folded: events=%d offset=%d", len(m.follow.events), m.follow.offset)
	}
}

// TestFollowToggle_SwitchesSourceAndResets asserts `t` flips
// transcript↔raw, resets the tail state (offset 0, buffers cleared, pinned),
// and re-targets the poll path; esc returns to the dashboard and the next
// followTickMsg dies (nil cmd) instead of polling forever.
func TestFollowToggle_SwitchesSourceAndResets(t *testing.T) {
	m := NewModel(WithLogsDir("/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})
	m, _ = m.Update(keyF)

	if got, want := m.followPath(), "/logs/CLI-1.transcript.jsonl"; got != want {
		t.Fatalf("followPath() = %q, want %q", got, want)
	}

	m, cmd := m.Update(keyT)
	if m.follow.source != followRaw {
		t.Errorf("after t source = %v, want followRaw", m.follow.source)
	}
	if got, want := m.followPath(), "/logs/CLI-1.log"; got != want {
		t.Errorf("followPath() = %q, want %q", got, want)
	}
	if m.follow.offset != 0 || len(m.follow.events) != 0 || !m.follow.pinned {
		t.Errorf("toggle did not reset tail state: %+v", m.follow)
	}
	if cmd == nil {
		t.Error("after t cmd = nil, want an immediate re-poll")
	}

	// While following, the tick keeps the loop alive… (stamped with the
	// session's generation, as scheduleFollowTick stamps the real ones)
	gen := m.followGen
	_, cmd = m.Update(followTickMsg{Gen: gen})
	if cmd == nil {
		t.Error("followTickMsg in follow mode returned nil cmd, want poll+reschedule")
	}
	// …and after esc, it dies.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.ViewMode(); got != "dashboard" {
		t.Fatalf("after esc ViewMode() = %q, want dashboard", got)
	}
	_, cmd = m.Update(followTickMsg{Gen: gen})
	if cmd != nil {
		t.Error("followTickMsg after leaving follow mode returned a cmd, want nil (tick chain must die)")
	}
}

// TestFollowTick_SupersededSessionDies asserts a tick from an earlier follow
// session is dropped even when follow mode is active again: f→esc→f inside
// one tick interval (normal key-repeat territory) must not leave the first
// session's chain alive alongside the second's — two concurrent 500ms chains
// polling one file and sharing one offset can duplicate lines. Every entry
// into follow mode bumps a generation; a tick carrying a stale generation
// returns nil cmd regardless of the current mode.
func TestFollowTick_SupersededSessionDies(t *testing.T) {
	m := NewModel(WithLogsDir("/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})

	m, _ = m.Update(keyF) // session 1
	gen1 := m.followGen
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = m.Update(keyF) // session 2, before session 1's tick fires

	if m.followGen == gen1 {
		t.Fatalf("re-entering follow did not bump the generation (still %d)", gen1)
	}
	// Session 1's outstanding tick arrives now: it must die even though
	// follow mode is (again) active.
	_, cmd := m.Update(followTickMsg{Gen: gen1})
	if cmd != nil {
		t.Error("superseded session's tick returned a cmd, want nil (chain must die)")
	}
	// Session 2's own tick still drives the loop.
	_, cmd = m.Update(followTickMsg{Gen: m.followGen})
	if cmd == nil {
		t.Error("current session's tick returned nil cmd, want poll+reschedule")
	}
}

// TestFollowView_FillsFrameHeight asserts the follow screen honors the
// whole-frame invariant every other mode holds.
func TestFollowView_FillsFrameHeight(t *testing.T) {
	m := NewModel(WithLogsDir("/logs"))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: followSnap()})
	m, _ = m.Update(keyF)

	if got := lipgloss.Height(m.View()); got != 40 {
		t.Errorf("follow View height = %d, want 40", got)
	}
}
