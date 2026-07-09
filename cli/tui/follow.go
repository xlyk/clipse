package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Follow mode (U1) tails one issue's on-disk agent logs in realtime. Two
// sources exist under <board>/logs/, both keyed by the issue identifier:
//
//   - <ISSUE>.transcript.jsonl — the structured DAC transcript the worker
//     appends (dispatcher.transcriptPath / clipse_agent.transcript), one
//     JSON object per line, append-only across every turn/lane/rework the
//     issue ever runs. Rendered semantically below.
//   - <ISSUE>.log — the worker's raw stderr (spawn.LocalSpawner), opened
//     O_TRUNC on every spawn, for the crashes and tracebacks the transcript
//     never sees. Rendered as plain dim lines.
//
// The tail is a tea.Cmd polling the active file every followInterval from a
// stored byte offset — I/O stays out of the pure Update, the same shape as
// the snapshot refresh. A file smaller than the stored offset means a new
// spawn truncated it (the stderr log) or a human replaced it (the
// transcript): reset the offset to 0 and rebuild.

// followSource selects which of the two log files follow mode tails.
type followSource int

const (
	followTranscript followSource = iota // structured JSONL (default)
	followRaw                            // worker stderr
)

// followInterval is the tail poll cadence — fast enough to feel live, slow
// enough that re-reading an appended tail is negligible I/O.
const followInterval = 500 * time.Millisecond

// transcriptEvent is one parsed transcript line: the union of every event
// type's fields (the writer stamps lane/run_id/model context onto every
// line, and each event type adds its own). A malformed line parses to the
// zero value with raw holding the original text, so the renderer can show it
// dim instead of crashing — a garbled line must never break the view.
type transcriptEvent struct {
	Event       string         `json:"event"`
	Lane        string         `json:"lane"`
	RunID       string         `json:"run_id"`
	Model       string         `json:"model"`
	TaskText    string         `json:"task_text"`
	Text        string         `json:"text"`
	Name        string         `json:"name"`
	Args        map[string]any `json:"args"`
	Status      string         `json:"status"`
	Content     string         `json:"content"`
	Payload     string         `json:"payload"`
	OutcomeHint string         `json:"outcome_hint"`
	TokensIn    int            `json:"tokens_in"`
	TokensOut   int            `json:"tokens_out"`
	Error       string         `json:"error"`
	Ts          float64        `json:"ts"`

	raw string // original line, kept for the malformed fallback
}

// parseTranscriptLine decodes one JSONL line, degrading a malformed line to
// a raw-text event rather than an error.
func parseTranscriptLine(line string) transcriptEvent {
	var ev transcriptEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return transcriptEvent{raw: line}
	}
	return ev
}

// followState is the model-side state of an active follow session: which
// issue and source are tailed, the byte offset the next poll reads from, the
// trailing partial line carried between polls, the accumulated parsed
// events (transcript) or raw lines (stderr), whether the viewport is pinned
// to the bottom, and the last poll error.
type followState struct {
	ident    string
	source   followSource
	offset   int64
	partial  []byte
	events   []transcriptEvent
	rawLines []string
	pinned   bool
	err      error
}

// applyChunk folds one poll's read into the buffers: reset first when the
// file shrank (a new spawn O_TRUNC'ed the stderr log), then split complete
// lines out of partial+data — carrying any trailing fragment to the next
// poll so a JSON object split across reads still parses whole.
func (f *followState) applyChunk(data []byte, newOffset int64, reset bool) {
	if reset {
		f.events = f.events[:0]
		f.rawLines = f.rawLines[:0]
		f.partial = nil
	}
	f.offset = newOffset
	buf := append(f.partial, data...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		line := string(buf[:i])
		buf = buf[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if f.source == followTranscript {
			f.events = append(f.events, parseTranscriptLine(line))
		} else {
			f.rawLines = append(f.rawLines, line)
		}
	}
	f.partial = append([]byte(nil), buf...)
}

// lane reports the most recent turn_start's bare lane — the agent currently
// (or last) driving this issue — used to color the follow header. Empty
// until a turn_start has parsed.
func (f followState) lane() string {
	for i := len(f.events) - 1; i >= 0; i-- {
		if f.events[i].Event == "turn_start" {
			return bareLane(f.events[i].Lane)
		}
	}
	return ""
}

// followTickMsg drives the tail poll cadence, mirroring TickMsg's shape:
// Update responds by returning the poll command plus the next tick — and by
// returning nothing once follow mode has been left or the tick's session has
// been superseded, which lets the stale chain die instead of polling forever.
type followTickMsg struct {
	// Gen identifies the follow session this tick was scheduled by (the
	// model's followGen at schedule time). Update drops a tick whose Gen no
	// longer matches, regardless of the current mode: a mode-only guard
	// would let f→esc→f inside one tick interval resurrect the first
	// session's chain alongside the second's — two concurrent 500ms chains
	// polling the same file and sharing one offset.
	Gen int
}

// scheduleFollowTick returns a tea.Cmd that fires a followTickMsg after
// followInterval, stamped with the scheduling session's generation.
func scheduleFollowTick(gen int) tea.Cmd {
	return tea.Tick(followInterval, func(time.Time) tea.Msg {
		return followTickMsg{Gen: gen}
	})
}

// followPollMsg carries one tail poll's read back into Update. Path echoes
// which file was read so a stale poll (issued before a source toggle or
// mode exit) can be recognized and dropped.
type followPollMsg struct {
	Path     string
	Data     []byte
	Offset   int64 // offset after this read; the next poll starts here
	Reset    bool  // file shrank below the stored offset — rebuild buffers
	NotExist bool  // file not written yet — keep polling, show placeholder
	Err      error
}

// pollFollowFile returns the tea.Cmd that reads path from offset to EOF —
// all the tailer's I/O lives here, never in Update. A file smaller than
// offset means a new spawn O_TRUNC'ed it: report Reset and re-read from 0.
// One poll's read is capped so a giant backlog can't stall a frame; the
// remainder arrives on the following ticks.
func pollFollowFile(path string, offset int64) tea.Cmd {
	return func() tea.Msg {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return followPollMsg{Path: path, NotExist: true}
		}
		if err != nil {
			return followPollMsg{Path: path, Err: err}
		}
		defer func() { _ = f.Close() }()

		st, err := f.Stat()
		if err != nil {
			return followPollMsg{Path: path, Err: err}
		}
		reset := false
		if st.Size() < offset {
			offset, reset = 0, true
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return followPollMsg{Path: path, Err: err}
		}
		const maxChunk = 512 * 1024
		data, err := io.ReadAll(io.LimitReader(f, maxChunk))
		if err != nil {
			return followPollMsg{Path: path, Err: err}
		}
		return followPollMsg{Path: path, Data: data, Offset: offset + int64(len(data)), Reset: reset}
	}
}

// Transcript render styles: result badges and the edit-diff line colors. Kept
// beside the renderer that uses them; the palette vars live in view.go.
var (
	okBadgeStyle   = lipgloss.NewStyle().Foreground(cGreen)
	failBadgeStyle = lipgloss.NewStyle().Bold(true).Foreground(cRed)
	addLineStyle   = lipgloss.NewStyle().Foreground(cGreen)
	delLineStyle   = lipgloss.NewStyle().Foreground(cRed)
	shellStyle     = lipgloss.NewStyle().Bold(true).Foreground(cGreen)
)

// toolOutcome is a matched tool_result's display data: its status and raw
// content (so the renderer can surface failures inline and count grep matches).
// A present toolOutcome means the call resolved; a missing one means the call
// is still pending (or was orphaned by a crash). No duration: the transcript
// emits a tool_result the instant its ToolMessage streams but buffers the
// tool_call until the NEXT round's model message flushes it (dac.py), so a
// call and its result are written back-to-back and their ts delta is flush lag,
// not execution time — an unrecoverable, misleading number.
type toolOutcome struct {
	status  string
	content string
}

// matchToolResults pairs each tool_call with a tool_result of the same tool
// name within the same turn. It is ORDER-AGNOSTIC: DAC emits a round's
// tool_results before it flushes that round's tool_calls (see dac.py's
// _accumulate_message_chunk / _flush_pending), so a result usually precedes
// its call — but a partial/last round can emit a call with no result yet.
// Maintaining a waiting queue for each side and pairing whichever arrives
// second handles both orders. Both queues reset on every turn_start, so a turn
// that ended with an unresolved call (crash, interrupt, ceiling abort) can't
// steal a later turn's same-named result. It returns the outcome per call event
// index and the set of result event indices that were consumed — an
// unconsumed result (its call scrolled away in a reset) still renders
// standalone.
func matchToolResults(events []transcriptEvent) (map[int]toolOutcome, map[int]bool) {
	outcomes := make(map[int]toolOutcome)
	consumed := make(map[int]bool)
	openCalls := make(map[string][]int)   // call indices awaiting a result
	openResults := make(map[string][]int) // result indices awaiting a call
	for i, ev := range events {
		switch ev.Event {
		case "turn_start":
			openCalls = make(map[string][]int)
			openResults = make(map[string][]int)
		case "tool_call":
			if q := openResults[ev.Name]; len(q) > 0 {
				ri := q[0]
				openResults[ev.Name] = q[1:]
				outcomes[i] = toolOutcome{status: events[ri].Status, content: events[ri].Content}
				consumed[ri] = true
			} else {
				openCalls[ev.Name] = append(openCalls[ev.Name], i)
			}
		case "tool_result":
			if q := openCalls[ev.Name]; len(q) > 0 {
				ci := q[0]
				openCalls[ev.Name] = q[1:]
				outcomes[ci] = toolOutcome{status: ev.Status, content: ev.Content}
				consumed[i] = true
			} else {
				openResults[ev.Name] = append(openResults[ev.Name], i)
			}
		}
	}
	return outcomes, consumed
}

// renderTranscriptLines renders parsed transcript events semantically, each
// turn drawn as one scannable block behind a lane-colored ▏ gutter:
// turn_start as a bold header rule (bare model name); tool_call as a
// kind-colored verb + resolved argument + a right-aligned result badge — with
// an edit's diff or a failed command's error tail surfaced beneath it;
// assistant text wrapped; interrupt as an amber notice; turn_end as a dim rule
// with outcome + tokens; anything unknown or malformed as dim raw text — never
// a crash.
func renderTranscriptLines(events []transcriptEvent, width int) []string {
	if width < 8 {
		width = 8
	}
	outcomes, consumed := matchToolResults(events)

	// innerW is the content width to the right of the gutter cell ("▏ ").
	innerW := maxInt(width-2, 6)

	curLane := ""
	var lines []string
	// gut prefixes one content line with the current turn's lane-colored gutter.
	gut := func(s string) string {
		return lipgloss.NewStyle().Foreground(laneColor(curLane)).Render("▏") + " " + s
	}
	emit := func(styled ...string) {
		for _, s := range styled {
			lines = append(lines, gut(s))
		}
	}

	for i, ev := range events {
		switch ev.Event {
		case "turn_start":
			curLane = bareLane(ev.Lane)
			head := fmt.Sprintf("▶ %s · run %s · %s", curLane, shortID(ev.RunID), bareModel(ev.Model))
			styled := lipgloss.NewStyle().Bold(true).Foreground(laneColor(curLane)).Render(truncatePlain(head, innerW))
			if fill := innerW - lipgloss.Width(styled) - 1; fill > 0 {
				styled += " " + ruleStyle.Render(strings.Repeat("─", fill))
			}
			lines = append(lines, "", gut(styled))

		case "tool_call":
			o, resolved := outcomes[i]
			emit(renderToolCall(ev, o, resolved, innerW)...)

		case "tool_result":
			if consumed[i] {
				continue // already shown under its call
			}
			// An orphaned result (its call scrolled out of a reset window):
			// render it standalone with the same failure-biased badge.
			verb, color := toolVerb(ev.Name)
			left := lipgloss.NewStyle().Foreground(color).Render("← " + verb)
			emit(padBetween(left, resultBadge(ev.Name, ev.Status, ev.Content), innerW))

		case "assistant":
			wrapped := lipgloss.NewStyle().Foreground(cText).Width(innerW).Render(ev.Text)
			lines = append(lines, gut(""))
			for _, l := range strings.Split(wrapped, "\n") {
				lines = append(lines, gut(l))
			}

		case "interrupt":
			emit(waitingStyle.Render(truncatePlain("⏸ interrupt · "+oneLine(ev.Payload), innerW)))

		case "turn_end":
			var line string
			if ev.Error != "" {
				line = "── turn crashed · " + oneLine(ev.Error)
			} else {
				line = fmt.Sprintf("── %s · ↓%s ↑%s", ev.OutcomeHint,
					humanizeTokens(ev.TokensIn), humanizeTokens(ev.TokensOut))
			}
			emit(dimStyle.Render(truncatePlain(line, innerW)))
			lines = append(lines, "")

		default:
			// Unknown event type or malformed line: dim raw, never a crash.
			text := ev.raw
			if text == "" {
				text = oneLine(ev.Event + " " + ev.Text + ev.Content)
			}
			emit(dimStyle.Render(truncatePlain(text, innerW)))
		}
	}
	return lines
}

// renderToolCall renders one tool_call as its verb/argument line plus any
// detail lines (an edit's diff, a failed command's error tail). resolved is
// false while the call is still pending (badge → dim "…"). Every returned line
// is at most width columns wide; the caller adds the lane gutter.
func renderToolCall(ev transcriptEvent, o toolOutcome, resolved bool, width int) []string {
	// Right-aligned result badge (or a pending marker).
	badge := dimStyle.Render("…")
	if resolved {
		badge = resultBadge(ev.Name, o.status, o.content)
	}
	budget := maxInt(width-lipgloss.Width(badge)-1, 8)

	var head string
	if isShellCall(ev) {
		cmd := oneLine(firstLine(argString(ev.Args, "command")))
		body := truncatePlain(cmd, maxInt(budget-2, 4))
		head = padBetween(shellStyle.Render("$")+" "+lipgloss.NewStyle().Foreground(cText).Render(body), badge, width)
	} else {
		verb, color := toolVerb(ev.Name)
		detail := toolDetail(ev, maxInt(budget-6, 4))
		left := lipgloss.NewStyle().Foreground(color).Render(fmt.Sprintf("%-5s", verb)) + " " +
			lipgloss.NewStyle().Foreground(cText).Render(detail)
		head = padBetween(left, badge, width)
	}
	lines := []string{head}

	// edit_file: show the actual change as a red/green diff.
	if ev.Name == "edit_file" {
		lines = append(lines, editDiffLines(argString(ev.Args, "old_string"), argString(ev.Args, "new_string"), width)...)
	}

	// A failed call surfaces its error tail inline (failure-biased); a passing
	// call stays a single line.
	if resolved && resultFailed(o.status, o.content) {
		for _, l := range failureTail(o.content, 3) {
			lines = append(lines, "   "+delLineStyle.Render(truncatePlain(l, maxInt(width-3, 4))))
		}
	}
	return lines
}

// isShellCall reports whether a tool_call is a shell invocation. DAC names the
// tool "execute", but any call carrying a command argument is rendered as a
// "$ <cmd>" shell line — more robust than matching one tool name.
func isShellCall(ev transcriptEvent) bool {
	return ev.Name == "execute" || ev.Name == "shell" || argString(ev.Args, "command") != ""
}

// toolVerb maps a tool name to a short display verb and its kind color:
// reads are cyan, search teal, writes amber, agent/meta purple/dim.
func toolVerb(name string) (string, lipgloss.AdaptiveColor) {
	switch name {
	case "read_file":
		return "read", cCyan
	case "ls":
		return "list", cCyan
	case "glob":
		return "glob", cCyan
	case "grep":
		return "grep", cTeal
	case "edit_file":
		return "edit", cAmber
	case "write_file":
		return "write", cAmber
	case "write_todos":
		return "todos", cDim
	case "task":
		return "task", cPurple
	case "":
		return "·", cDim
	default:
		return name, cDim
	}
}

// toolDetail resolves a tool_call's arguments into a human-readable target,
// elided to width: a repo-relative path for file tools, a quoted pattern +
// scope for grep/glob, a count for write_todos, a description for task.
func toolDetail(ev transcriptEvent, width int) string {
	switch ev.Name {
	case "read_file", "ls", "edit_file", "write_file":
		p := argString(ev.Args, "file_path")
		if p == "" {
			p = argString(ev.Args, "path")
		}
		return shortPath(p, width)
	case "grep", "glob":
		pat := argString(ev.Args, "pattern")
		scope := shortPath(argString(ev.Args, "path"), maxInt(width/2, 8))
		q := `"` + pat + `"`
		if scope != "" {
			return truncatePlain(q+" in "+scope, width)
		}
		return truncatePlain(q, width)
	case "write_todos":
		if todos, ok := ev.Args["todos"].([]any); ok {
			return fmt.Sprintf("%d items", len(todos))
		}
		return "updated"
	case "task":
		d := oneLine(argString(ev.Args, "description"))
		if st := argString(ev.Args, "subagent_type"); st != "" {
			d = st + ": " + d
		}
		return truncatePlain(d, width)
	default:
		return truncatePlain(oneLine(argString(ev.Args, "path")), width)
	}
}

// editDiffLines renders an edit_file's old_string/new_string as an indented
// red/green line diff, capped so a large edit can't flood the pane.
func editDiffLines(old, new string, width int) []string {
	const maxSide = 6
	textW := maxInt(width-5, 4) // 3-space indent + "- "/"+ " marker
	var out []string
	add := func(prefix string, style lipgloss.Style, body string) {
		lines := strings.Split(body, "\n")
		shown := lines
		if len(shown) > maxSide {
			shown = shown[:maxSide]
		}
		for _, l := range shown {
			out = append(out, "   "+style.Render(prefix+truncatePlain(l, textW)))
		}
		if len(lines) > maxSide {
			out = append(out, "   "+dimStyle.Render(fmt.Sprintf("… %d more", len(lines)-maxSide)))
		}
	}
	if old != "" {
		add("- ", delLineStyle, old)
	}
	if new != "" {
		add("+ ", addLineStyle, new)
	}
	return out
}

// resultBadge builds the right-aligned status badge for a resolved tool call.
// It is failure-biased: for a shell command it reads the REAL exit code out of
// the content (DAC reports status="success" even when the wrapped command
// exited non-zero), so a failing command reads ✗, never ✓.
func resultBadge(name, status, content string) string {
	if resultFailed(status, content) {
		plain := "✗"
		if code, ok := execExitCode(content); ok {
			plain = fmt.Sprintf("✗ exit %d", code)
		}
		return failBadgeStyle.Render(plain)
	}
	plain := "✓"
	if name == "grep" {
		if n := grepMatchCount(content); n > 0 {
			plain = fmt.Sprintf("✓ %d matches", n)
		}
	}
	return okBadgeStyle.Render(plain)
}

// resultFailed decides whether a tool_result represents a failure. An explicit
// error status always fails; for a shell command the tool-wrapper status is
// unreliable (it reports success as long as the wrapper itself ran), so the
// real verdict comes from the exit-code marker embedded in the content — only
// shell results carry it, so this is safe for every other tool.
func resultFailed(status, content string) bool {
	if status == "error" {
		return true
	}
	if code, ok := execExitCode(content); ok {
		return code != 0
	}
	return false
}

// execExitCode extracts a shell command's exit code from an execute result's
// content. DAC appends a "[Command succeeded/failed with exit code N]" marker;
// a bare "Exit code: N" line is the fallback. Returns ok=false when neither is
// present (e.g. a still-streaming result).
func execExitCode(content string) (int, bool) {
	for _, marker := range []string{"[Command failed with exit code ", "[Command succeeded with exit code "} {
		if i := strings.LastIndex(content, marker); i >= 0 {
			rest := content[i+len(marker):]
			if j := strings.IndexByte(rest, ']'); j >= 0 {
				if n, err := strconv.Atoi(strings.TrimSpace(rest[:j])); err == nil {
					return n, true
				}
			}
		}
	}
	if i := strings.LastIndex(content, "Exit code:"); i >= 0 {
		rest := content[i+len("Exit code:"):]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[:j]
		}
		if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// failureTail returns the last few meaningful lines of a failed result's
// content — the actual error/traceback — with the [stderr] prefixes and the
// bookkeeping markers stripped. It keeps the tail (where the exception lands),
// not the head.
func failureTail(content string, max int) []string {
	var meaningful []string
	for _, ln := range strings.Split(content, "\n") {
		s := strings.TrimSpace(ln)
		s = strings.TrimSpace(strings.TrimPrefix(s, "[stderr]"))
		s = strings.TrimSpace(strings.TrimPrefix(s, "[stdout]"))
		if s == "" || strings.HasPrefix(s, "[Command ") || strings.HasPrefix(s, "Exit code:") {
			continue
		}
		meaningful = append(meaningful, s)
	}
	if len(meaningful) > max {
		meaningful = meaningful[len(meaningful)-max:]
	}
	return meaningful
}

// grepMatchCount counts hit lines in a grep result — lines beginning (after
// optional indent) with a line number followed by ':' or '-', the ripgrep/grep
// -n format. Best-effort and display-only; header lines and blanks don't count.
func grepMatchCount(content string) int {
	n := 0
	for _, ln := range strings.Split(content, "\n") {
		s := strings.TrimLeft(ln, " \t")
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i > 0 && i < len(s) && (s[i] == ':' || s[i] == '-') {
			n++
		}
	}
	return n
}

// relPath strips clipse's worktree prefix from an absolute path so a tool's
// argument renders repo-relative. Every worktree is rooted at
// <board>/worktrees/<slug>/ (a kernel invariant — internal/spawn), so
// everything after that slug is the repo-relative path.
func relPath(p string) string {
	const marker = "/worktrees/"
	if i := strings.Index(p, marker); i >= 0 {
		rest := p[i+len(marker):]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j+1:]
		}
		// The path IS the worktree root (a tool scoped to the whole repo);
		// show "." rather than the long, meaningless worktree-slug directory.
		return "."
	}
	return p
}

// shortPath relativizes p (relPath) then middle-elides it to max runes,
// preserving the leading directory and the trailing filename (the two ends
// that identify a file) with "…" standing in for the dropped middle.
func shortPath(p string, max int) string {
	if max <= 0 || p == "" {
		return ""
	}
	rel := relPath(p)
	if len([]rune(rel)) <= max {
		return rel
	}
	segs := strings.Split(rel, "/")
	if len(segs) >= 3 {
		head, tail := segs[0], segs[len(segs)-1]
		cand := tail
		for i := len(segs) - 2; i >= 1; i-- {
			next := segs[i] + "/" + cand
			if len([]rune(head+"/…/"+next)) > max {
				break
			}
			cand = next
		}
		if out := head + "/…/" + cand; len([]rune(out)) <= max {
			return out
		}
	}
	// Deep single filename (or no room to elide): keep the tail, drop the front.
	r := []rune(rel)
	return "…" + string(r[len(r)-(max-1):])
}

// argString reads a string-typed tool argument, returning "" for a missing or
// non-string value.
func argString(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	s, _ := args[key].(string)
	return s
}

// firstLine returns s up to (not including) its first newline.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// bareModel strips a "provider:" prefix from a model spec ("anthropic:claude-
// opus-4-6" → "claude-opus-4-6") so the turn header reads compactly.
func bareModel(model string) string {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		return model[i+1:]
	}
	return model
}

// renderRawLines renders the stderr tail: plain dim monospace, truncated to
// the pane width, no parsing.
func renderRawLines(raw []string, width int) []string {
	if width < 8 {
		width = 8
	}
	lines := make([]string, 0, len(raw))
	for _, l := range raw {
		lines = append(lines, dimStyle.Render(truncatePlain(l, width)))
	}
	return lines
}
