package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
// turn_start → a lane-named header line; tool_call → a dim "$ cmd" one-liner
// that gains "→ <status> · <dur>s" once its result arrives; assistant text →
// wrapped; turn_end → an outcome + token rule; malformed → dim raw text.
func TestRenderTranscriptLines(t *testing.T) {
	events := []transcriptEvent{
		{Event: "turn_start", Lane: "reviewer", RunID: "8494b1cc1690b9e368059c9db9d6717c", Model: "anthropic:claude-opus-4-6", Ts: 100},
		{Event: "tool_call", Name: "shell", Args: map[string]any{"command": "go test ./..."}, Ts: 101},
		{Event: "tool_result", Name: "shell", Status: "success", Content: "ok", Ts: 103.1},
		{Event: "assistant", Text: "All tests pass. Verifying the constraint next.", Ts: 104},
		{Event: "tool_call", Name: "read_file", Args: map[string]any{"path": "x.go"}, Ts: 105},
		{Event: "turn_end", OutcomeHint: "completed", TokensIn: 489800, TokensOut: 4800, Ts: 120},
		{raw: "not json"},
	}

	out := strings.Join(renderTranscriptLines(events, 80), "\n")

	if !strings.Contains(out, "reviewer") {
		t.Errorf("output missing the turn_start lane header:\n%s", out)
	}
	if !strings.Contains(out, "$ go test ./...") {
		t.Errorf("output missing the tool_call one-liner:\n%s", out)
	}
	if !strings.Contains(out, "→ success · 2.1s") {
		t.Errorf("output missing the matched result status/duration:\n%s", out)
	}
	if !strings.Contains(out, "All tests pass.") {
		t.Errorf("output missing the assistant text:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Errorf("output missing the unanswered tool_call (should render with a pending marker):\n%s", out)
	}
	if !strings.Contains(out, "↓489.8k") || !strings.Contains(out, "↑4.8k") {
		t.Errorf("output missing turn_end tokens:\n%s", out)
	}
	if !strings.Contains(out, "not json") {
		t.Errorf("output missing the malformed line's raw fallback:\n%s", out)
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
	if o, ok := outcomes[4]; !ok || o.status != "success" || o.dur != 2.5 {
		t.Errorf("turn 2's call (index 4) outcome = %+v ok=%v, want success · 2.5s", o, ok)
	}
	if !consumed[5] {
		t.Errorf("turn 2's result (index 5) not consumed, want consumed by its own turn's call")
	}

	out := strings.Join(renderTranscriptLines(events, 80), "\n")
	if !strings.Contains(out, "$ go build ./...  → …") {
		t.Errorf("crashed turn's call must render unresolved:\n%s", out)
	}
	if !strings.Contains(out, "$ go test ./...  → success · 2.5s") {
		t.Errorf("later turn's call must pair with its own result:\n%s", out)
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
