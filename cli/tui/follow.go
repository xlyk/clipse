package tui

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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
// returning nothing once follow mode has been left, which lets the tick
// chain die instead of polling forever.
type followTickMsg struct{}

// scheduleFollowTick returns a tea.Cmd that fires a followTickMsg after
// followInterval.
func scheduleFollowTick() tea.Cmd {
	return tea.Tick(followInterval, func(time.Time) tea.Msg {
		return followTickMsg{}
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

// toolOutcome is a matched tool_result's display data: its status plus the
// call→result ts delta (seconds; < 0 when unknown).
type toolOutcome struct {
	status string
	dur    float64
}

// matchToolResults pairs each tool_call with the nearest following
// tool_result of the same tool name (FIFO per name, each result consumed at
// most once). Pairing is scoped to a single turn: the open queues reset on
// every turn_start, so a turn that ended with an unresolved call (crash,
// interrupt, ceiling abort — the worker flushes the calls even when the
// ToolNode never ran) can't steal a later turn's same-named result. It
// returns the outcome per call event index and the set of result event
// indices that were consumed — an unconsumed result (its call scrolled away
// in a reset) still renders standalone.
func matchToolResults(events []transcriptEvent) (map[int]toolOutcome, map[int]bool) {
	outcomes := make(map[int]toolOutcome)
	consumed := make(map[int]bool)
	open := make(map[string][]int)
	for i, ev := range events {
		switch ev.Event {
		case "turn_start":
			open = make(map[string][]int)
		case "tool_call":
			open[ev.Name] = append(open[ev.Name], i)
		case "tool_result":
			q := open[ev.Name]
			if len(q) == 0 {
				continue
			}
			ci := q[0]
			open[ev.Name] = q[1:]
			dur := ev.Ts - events[ci].Ts
			if dur < 0 {
				dur = -1
			}
			outcomes[ci] = toolOutcome{status: ev.Status, dur: dur}
			consumed[i] = true
		}
	}
	return outcomes, consumed
}

// renderTranscriptLines renders parsed transcript events semantically:
// turn_start as a lane-colored header rule; tool_call as a dim "$ <cmd>"
// one-liner carrying its matched result's status/duration (or a pending
// marker); assistant text wrapped at full width; interrupt as an amber
// notice; turn_end as a dim rule with outcome + tokens (or the crash error);
// anything unknown or malformed as dim raw text — never a crash.
func renderTranscriptLines(events []transcriptEvent, width int) []string {
	if width < 8 {
		width = 8
	}
	outcomes, consumed := matchToolResults(events)

	var lines []string
	for i, ev := range events {
		switch ev.Event {
		case "turn_start":
			head := fmt.Sprintf("▶ %s · run %s · %s", bareLane(ev.Lane), shortID(ev.RunID), ev.Model)
			styled := lipgloss.NewStyle().Bold(true).Foreground(laneColor(bareLane(ev.Lane))).Render(truncatePlain(head, width))
			if fill := width - lipgloss.Width(styled) - 1; fill > 0 {
				styled += " " + ruleStyle.Render(strings.Repeat("─", fill))
			}
			lines = append(lines, "", styled)

		case "tool_call":
			label := ev.Name
			if cmd, ok := ev.Args["command"].(string); ok && cmd != "" {
				label = cmd
			}
			line := "$ " + oneLine(label)
			if o, ok := outcomes[i]; ok {
				status := o.status
				if status == "" {
					status = "done"
				}
				line += "  → " + status
				if o.dur >= 0 {
					line += fmt.Sprintf(" · %.1fs", o.dur)
				}
			} else {
				line += "  → …"
			}
			lines = append(lines, dimStyle.Render(truncatePlain(line, width)))

		case "tool_result":
			if consumed[i] {
				continue // already shown on its call's line
			}
			lines = append(lines, dimStyle.Render(truncatePlain("→ "+ev.Name+": "+ev.Status, width)))

		case "assistant":
			wrapped := lipgloss.NewStyle().Foreground(cText).Width(width).Render(ev.Text)
			lines = append(lines, "")
			lines = append(lines, strings.Split(wrapped, "\n")...)

		case "interrupt":
			lines = append(lines, waitingStyle.Render(truncatePlain("⏸ interrupt · "+oneLine(ev.Payload), width)))

		case "turn_end":
			var line string
			if ev.Error != "" {
				line = "── turn crashed · " + oneLine(ev.Error)
			} else {
				line = fmt.Sprintf("── turn end · %s · ↓%s ↑%s", ev.OutcomeHint,
					humanizeTokens(ev.TokensIn), humanizeTokens(ev.TokensOut))
			}
			lines = append(lines, dimStyle.Render(truncatePlain(line, width)), "")

		default:
			// Unknown event type or malformed line: dim raw, never a crash.
			text := ev.raw
			if text == "" {
				text = oneLine(ev.Event + " " + ev.Text + ev.Content)
			}
			lines = append(lines, dimStyle.Render(truncatePlain(text, width)))
		}
	}
	return lines
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
