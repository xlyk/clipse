# TUI v2 Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal**: Ship the approved TUI v2 core scope (`docs/design/2026-07-08-tui-v2-core.md`) — follow mode (U1) plus the polish tier (P1–P6) — as one draft PR touching only `cli/tui/` and `cli/tui.go`.

**Architecture**: The TUI stays a pure-`Update` bubbletea model fed by injected `tea.Cmd`s: snapshots arrive via `SnapshotMsg` from the refresh command wired in `cli/tui.go`, wall-clock enters only in `View`, and all new I/O (the follow-mode file tail, the transcript-existence directory listing) lives in `tea.Cmd`s / the refresh closure, never in `Update`. Tasks 1–6 are model/view-only reshapes of existing render paths; tasks 7–8 add a new `follow.go` (pure tail/parse/render logic + one polling `tea.Cmd`) and wire it into the model, keys, and `runTUI`.

**Tech Stack**: Go, `charmbracelet/bubbletea` + `lipgloss` + `bubbles` (viewport/progress/help/key — all already in the module graph), standard library only otherwise. No Python changes.

**Global Constraints** (binding, from the approved spec):

- "No new Go module dependencies in this PR. bubbles submodules already in the module graph are fine."
- "Visual direction (draft §3): lane color is identity; verdicts are the only loud elements; density over chrome; GitHub-dark stays the dark palette."
- "House TUI style holds: pure `Update`, I/O only in `tea.Cmd`s, wall-clock only in `View`, injected refresh, unit tests without TTY/DB."
- "Kernel untouched: `cli/tui/` (+ `cli/tui.go` wiring if needed) only."
- "TDD per item where the logic is pure (layout math, feed classification, tail-offset handling, chip rendering); `make test` + `make lint` green."
- Commits: Conventional Commits, casual/lowercase, no trailing period, no AI/Claude signature. Never `git add -A` / `git add .` — always explicit file paths.
- After every task: `go test ./cli/tui/ -race` and `make lint` must pass before committing.

**Task order** (each lands green independently): P5 palette first (every later task styles through it), then the P1→P2→P3→P4→P6 reshapes, then U1 in two tasks (pure tailer logic, then UI integration). The spec ranks U1 first in *value*; this order optimizes safe incremental landing — the P-tasks churn functions U1's view reuses (`panelBox`, `laneColor`, palette types), so building U1 on the settled versions avoids rework.

Line anchors below are as of commit `b1a6322`; if a file has drifted, match on the quoted code, not the line numbers.

---

## Task 1 — P5: adaptive light/dark palette

Every `lipgloss.Color` constant becomes a `lipgloss.AdaptiveColor{Light, Dark}` with the current GitHub-dark hex as `Dark` and a GitHub-light analogue as `Light`. Every function that names the color type in a signature changes with it. This is the foundation task: all later tasks reference these palette vars.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — palette const block (lines 15–28), `section.accent` field (line 80), `panelBox` (line 191), `countChip` (line 419), `laneColor` (line 425), `statusColor` (line 458).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/activity.go` — `eventGlyph` return type (line 106).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `NewModel`'s progress gradient (line 190: `progress.New(progress.WithGradient(string(cCyan), string(cGreen)), progress.WithoutPercentage())` — `string(...)` conversion no longer compiles on a struct).
- Create: `/Users/xlyk/Code/clipse/cli/tui/view_internal_test.go` — palette pin test.

**Interfaces produced** (later tasks depend on these exact names):
- Palette vars `cText, cDim, cBorder, cInk, cGreen, cCyan, cRed, cAmber, cPurple, cTeal, cOrange` of type `lipgloss.AdaptiveColor`.
- `func laneColor(lane string) lipgloss.AdaptiveColor`
- `func statusColor(status string) lipgloss.AdaptiveColor`
- `func countChip(glyph, label string, n int, accent lipgloss.AdaptiveColor) string`
- `func panelBox(title string, accent lipgloss.AdaptiveColor, body string, colW, totalH int) string`
- `func eventGlyph(kind string) (string, lipgloss.AdaptiveColor)`
- `func progressGradientColors() (string, string)`

**Steps**

- [ ] Write the failing test. Create `/Users/xlyk/Code/clipse/cli/tui/view_internal_test.go`:

```go
package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPaletteAdaptive pins the adaptive palette (P5): every color must carry
// the original GitHub-dark hex as its Dark variant (so dark terminals are
// pixel-identical to v1) and a distinct, non-empty GitHub-light analogue as
// Light. A missing Light value would silently render the dark hex on light
// backgrounds — exactly the failure mode AdaptiveColor exists to prevent.
func TestPaletteAdaptive(t *testing.T) {
	tests := []struct {
		name string
		c    lipgloss.AdaptiveColor
		dark string // the original v1 dark-palette hex, pinned
	}{
		{"cText", cText, "#c9d1d9"},
		{"cDim", cDim, "#6e7681"},
		{"cBorder", cBorder, "#30363d"},
		{"cInk", cInk, "#0d1117"},
		{"cGreen", cGreen, "#3fb950"},
		{"cCyan", cCyan, "#58a6ff"},
		{"cRed", cRed, "#f85149"},
		{"cAmber", cAmber, "#d29922"},
		{"cPurple", cPurple, "#bc8cff"},
		{"cTeal", cTeal, "#39c5cf"},
		{"cOrange", cOrange, "#db6d28"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.c.Dark != tt.dark {
				t.Errorf("%s.Dark = %q, want the original dark hex %q", tt.name, tt.c.Dark, tt.dark)
			}
			if tt.c.Light == "" {
				t.Errorf("%s.Light is empty, want a GitHub-light analogue", tt.name)
			}
			if tt.c.Light == tt.c.Dark {
				t.Errorf("%s.Light == Dark (%q), want a distinct light-mode value", tt.name, tt.c.Light)
			}
		})
	}
}

// TestLaneColorAdaptive asserts the lane-identity mapping survives the type
// change: each lane keeps its established dark hue.
func TestLaneColorAdaptive(t *testing.T) {
	tests := []struct {
		lane string
		dark string
	}{
		{"coder", "#58a6ff"},
		{"reviewer", "#bc8cff"},
		{"git_operator", "#db6d28"},
		{"unknown", "#6e7681"},
	}
	for _, tt := range tests {
		if got := laneColor(tt.lane); got.Dark != tt.dark {
			t.Errorf("laneColor(%q).Dark = %q, want %q", tt.lane, got.Dark, tt.dark)
		}
	}
}
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestPaletteAdaptive|TestLaneColorAdaptive'` — expect a **compile error** (`cText` is a `lipgloss.Color`, not `lipgloss.AdaptiveColor`). That is the failing state.

- [ ] Implement. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, replace the palette const block (currently lines 14–28, `// Palette — a GitHub-dark-ish scheme …` through the closing `)`) with:

```go
// Palette — adaptive light/dark (P5). Dark is the original GitHub-dark
// scheme (unchanged, so dark terminals render pixel-identical to v1); Light
// is the GitHub-light analogue of each hue, picked so the same semantic
// reads on a white background. lipgloss resolves the pair against the
// terminal's detected background per render. Truecolor hex degrades
// gracefully on 256-color terminals.
var (
	cText   = lipgloss.AdaptiveColor{Light: "#1f2328", Dark: "#c9d1d9"}
	cDim    = lipgloss.AdaptiveColor{Light: "#57606a", Dark: "#6e7681"}
	cBorder = lipgloss.AdaptiveColor{Light: "#d0d7de", Dark: "#30363d"}
	// cInk is the text color painted ON a bright accent badge: near-black on
	// the pale dark-mode accents, white on the deeper light-mode accents.
	cInk = lipgloss.AdaptiveColor{Light: "#ffffff", Dark: "#0d1117"}

	cGreen  = lipgloss.AdaptiveColor{Light: "#1a7f37", Dark: "#3fb950"}
	cCyan   = lipgloss.AdaptiveColor{Light: "#0969da", Dark: "#58a6ff"}
	cRed    = lipgloss.AdaptiveColor{Light: "#cf222e", Dark: "#f85149"}
	cAmber  = lipgloss.AdaptiveColor{Light: "#9a6700", Dark: "#d29922"}
	cPurple = lipgloss.AdaptiveColor{Light: "#8250df", Dark: "#bc8cff"}
	cTeal   = lipgloss.AdaptiveColor{Light: "#1b7c83", Dark: "#39c5cf"}
	cOrange = lipgloss.AdaptiveColor{Light: "#bc4c00", Dark: "#db6d28"}
)
```

- [ ] In the same file, update the four signatures that name the color type. `section.accent` (line 80):

```go
	accent lipgloss.AdaptiveColor
```

`panelBox` (line 191):

```go
func panelBox(title string, accent lipgloss.AdaptiveColor, body string, colW, totalH int) string {
```

`countChip` (line 419):

```go
func countChip(glyph, label string, n int, accent lipgloss.AdaptiveColor) string {
```

`laneColor` (line 425) and `statusColor` (line 458):

```go
func laneColor(lane string) lipgloss.AdaptiveColor {
```

```go
func statusColor(status string) lipgloss.AdaptiveColor {
```

(Bodies unchanged — `Foreground`/`Background`/`BorderForeground` accept any `lipgloss.TerminalColor`, which `AdaptiveColor` implements.)

- [ ] Still in `view.go`, add the gradient helper right after the palette var block (the bubbles progress gradient needs two concrete hex strings — it lerps between them — so the adaptive pair must be resolved once, at model construction):

```go
// progressGradientColors resolves the header progress bar's gradient pair
// (cyan → green) against the terminal background once, at model
// construction. progress.WithGradient needs concrete hex strings to lerp
// between, so the adaptive palette can't be passed through directly. In a
// non-TTY context (tests) lipgloss reports a light background and the light
// pair is used — harmless, since styles render unstyled there anyway.
func progressGradientColors() (string, string) {
	if lipgloss.HasDarkBackground() {
		return cCyan.Dark, cGreen.Dark
	}
	return cCyan.Light, cGreen.Light
}
```

- [ ] In `/Users/xlyk/Code/clipse/cli/tui/activity.go`, change `eventGlyph`'s signature (line 106) — body unchanged:

```go
func eventGlyph(kind string) (string, lipgloss.AdaptiveColor) {
```

- [ ] In `/Users/xlyk/Code/clipse/cli/tui/model.go`, replace the `progress:` line inside `NewModel` (line 190):

```go
		progress: progress.New(progress.WithGradient(progressGradientColors()), progress.WithoutPercentage()),
```

(`progress.WithGradient(colorA, colorB string)` accepts the two-value return directly.)

- [ ] Run to green: `go test ./cli/tui/ -race` — all tests pass. Then `make lint` — clean.

- [ ] Commit:

```
git add cli/tui/view.go cli/tui/activity.go cli/tui/model.go cli/tui/view_internal_test.go
git commit -m "feat(tui): adaptive light/dark palette"
```

---

## Task 2 — P1: content-sized pipeline, activity absorbs the rest, empty sections dropped

Invert the vertical split in `dims()`: the PIPELINE panel takes `min(natural rendered height, bodyH − actMin)` and the ACTIVITY feed absorbs every remaining row. Sections with zero rows disappear from the body entirely (their zero-counts already live in the header chips), killing the `· none` lines.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — `dims()` (lines 111–139).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sections.go` — `renderBody` (lines 27–37), `renderSection` (lines 43–62), `orderedLineIndex` (lines 180–197).
- Create: `/Users/xlyk/Code/clipse/cli/tui/layout_internal_test.go`.

**Interfaces**: consumes the Task 1 palette (`dimStyle`, etc. — unchanged names). Produces no new symbols; changes `dims()` semantics that Task 8's follow view reads (`d.headerH`, `d.footerH`, `d.cw`, `d.frameH` are untouched). `renderBody` gains the invariant "empty sections are skipped", which `orderedLineIndex` mirrors — the selection-follow geometry depends on the two never drifting.

**Steps**

- [ ] Write the failing tests. Create `/Users/xlyk/Code/clipse/cli/tui/layout_internal_test.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// snapWithNTodo builds a snapshot of n "todo" issues — a sparse board whose
// pipeline content is much shorter than a tall terminal's body.
func snapWithNTodo(n int) store.Snapshot {
	issues := make([]store.IssueSnapshot, 0, n)
	for i := 0; i < n; i++ {
		issues = append(issues, store.IssueSnapshot{
			Issue: store.Issue{
				ID:          fmt.Sprintf("id-%02d", i),
				Identifier:  fmt.Sprintf("CLI-%02d", i),
				LaneLabel:   "coder",
				BoardStatus: "todo",
			},
		})
	}
	return store.Snapshot{Issues: issues}
}

// TestDims_PipelineContentSizedOnTallTerminal asserts the P1 reflow: on a
// tall terminal with a sparse board (10 cards), the pipeline panel shrinks
// to its natural content height and the activity feed absorbs every
// remaining body row — well past the old fixed 18-row clamp.
func TestDims_PipelineContentSizedOnTallTerminal(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(10)})

	d := m.dims()
	natural := lipgloss.Height(m.renderBody(d.pipeTextW, 0)) + 3 // border(2) + title(1)
	if d.pipeH != natural {
		t.Errorf("pipeH = %d, want natural content height %d (content-sized pipeline)", d.pipeH, natural)
	}
	if got, want := d.actH, d.bodyH-d.pipeH; got != want {
		t.Errorf("actH = %d, want the full remainder %d (activity absorbs spare rows)", got, want)
	}
	if d.actH <= 18 {
		t.Errorf("actH = %d, want > 18 (the old fixed clamp) on a tall sparse board", d.actH)
	}
	if d.pipeH+d.actH != d.bodyH {
		t.Errorf("pipeH(%d) + actH(%d) != bodyH(%d) — panels must tile the body exactly", d.pipeH, d.actH, d.bodyH)
	}
}

// TestDims_PipelineCappedLeavesActivityMinimum asserts the other direction:
// a full board caps the pipeline at bodyH − actMin so the feed always keeps
// its minimum band, and the pipeline viewport scrolls the overflow.
func TestDims_PipelineCappedLeavesActivityMinimum(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(40)})

	d := m.dims()
	if got, want := d.pipeH, d.bodyH-6; got != want {
		t.Errorf("pipeH = %d, want bodyH−actMin = %d when content overflows", got, want)
	}
	if got, want := d.actH, 6; got != want {
		t.Errorf("actH = %d, want the actMin floor %d", got, want)
	}
}

// TestRenderBody_OmitsEmptySections asserts empty sections vanish from the
// body — no "· none" filler, no empty band headings; their zero-counts live
// in the header chips instead.
func TestRenderBody_OmitsEmptySections(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: snapWithNTodo(3)})

	body := m.renderBody(100, 0)
	if strings.Contains(body, "· none") {
		t.Errorf("renderBody still renders %q filler:\n%s", "· none", body)
	}
	if strings.Contains(body, "BLOCKED") {
		t.Errorf("renderBody renders an empty BLOCKED band:\n%s", body)
	}
	if !strings.Contains(body, "QUEUED") {
		t.Errorf("renderBody dropped the non-empty QUEUED band:\n%s", body)
	}
}

// TestRenderBody_EmptyBoardPlaceholder asserts a board with no issues still
// renders something (never an empty string, which would collapse the panel).
func TestRenderBody_EmptyBoardPlaceholder(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	body := m.renderBody(100, 0)
	if !strings.Contains(body, "no issues") {
		t.Errorf("empty-board renderBody = %q, want a %q placeholder", body, "no issues")
	}
}
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestDims_|TestRenderBody_'` — expect failures: `pipeH` follows the old `bodyH−actH` math and the body still contains `· none` / `BLOCKED`.

- [ ] Implement `dims()`. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, replace the body of `dims()` from the comment `// Panels stack vertically: …` through `d.actVpH = maxInt(d.actH-3, 1)` (currently lines 128–137) so the whole function reads:

```go
// dims computes the frame geometry. It is pure (now=0 into the measured
// helpers) so it is safe to call from both Update's layout() and View.
func (m Model) dims() layoutDims {
	w := m.width
	if w <= 0 {
		w = 120
	}
	h := m.height
	if h <= 0 {
		h = 40
	}
	d := layoutDims{frameW: w, frameH: h}
	d.cw = maxInt(w-2, 24)

	d.headerH = lipgloss.Height(m.renderHeader(d.cw, 0))
	d.tabsH = lipgloss.Height(m.renderTabs(d.cw))
	d.footerH = lipgloss.Height(m.renderFooter(d.cw))
	d.bodyH = maxInt(h-d.headerH-d.tabsH-d.footerH, 6)

	// Panels stack vertically: PIPELINE on top, the ACTIVITY feed as a
	// full-width band below it. The pipeline is content-sized — it takes
	// min(its natural rendered height, bodyH − actMin) — and the activity
	// feed absorbs every remaining row (P1). A sparse board therefore gives
	// its spare height to the live feed instead of rendering void inside the
	// pipeline border; a full board naturally wins the space back, so the
	// layout is self-balancing.
	const actMin = 6
	d.pipeTextW = maxInt(d.cw-2, 8)
	d.actTextW = maxInt(d.cw-2, 8)
	natural := lipgloss.Height(m.renderBody(d.pipeTextW, 0)) + 3 // + border(2) + title(1)
	d.pipeH = clampInt(natural, 4, maxInt(d.bodyH-actMin, 4))
	d.actH = maxInt(d.bodyH-d.pipeH, actMin)
	d.pipeVpH = maxInt(d.pipeH-3, 1) // border(2) + title(1)
	d.actVpH = maxInt(d.actH-3, 1)
	return d
}
```

- [ ] Implement the empty-section skip. In `/Users/xlyk/Code/clipse/cli/tui/sections.go`, replace `renderBody` (lines 23–37 including its doc comment) with:

```go
// renderBody renders the scrollable dashboard body: the non-empty section
// panels stacked, then a compact DONE summary. Empty sections are omitted
// entirely — their zero-counts already live in the header chips — so a
// sparse board never spends rows saying "none" (P1). now feeds the live
// rows' elapsed (View passes the wall clock; layout passes 0 for a stable
// line count, since elapsed is inline and never adds lines).
func (m Model) renderBody(inner int, now int64) string {
	var parts []string
	for _, s := range m.sectionList() {
		if len(s.rows) == 0 {
			continue
		}
		parts = append(parts, m.renderSection(s, inner, now))
	}
	body := strings.Join(parts, "\n")
	if done := m.renderDoneSummary(inner); done != "" {
		if body != "" {
			body += "\n"
		}
		body += done
	}
	if body == "" {
		return dimStyle.Render("no issues on the board yet")
	}
	return body
}
```

- [ ] In the same file, drop the `· none` branch from `renderSection` — replace its `lines := …` block (currently lines 52–60) with:

```go
	lines := []string{head}
	for _, row := range s.rows {
		lines = append(lines, m.renderRow(row, s, inner, now))
	}
	lines = append(lines, "") // trailing spacer for breathing room between groups
```

and update its doc comment's first sentence to match:

```go
// renderSection renders one titled group of rows, tinted with the section's
// accent color. Callers skip empty sections (renderBody / orderedLineIndex),
// so there is no empty-placeholder branch. It is borderless — the enclosing
// PIPELINE panel supplies the single frame — so the groups read as one board
// rather than separate boxes.
```

- [ ] Mirror the skip in `orderedLineIndex` (same file, lines 180–197) — the geometry must match `renderBody` exactly or selection-follow scrolling drifts:

```go
// orderedLineIndex returns the 0-based line, within renderBody's output, of the
// ordered row at global index g. It measures actual rendered heights rather
// than assuming one line per row, so a row that wraps at a narrow width can't
// drift the result: preceding groups are summed via lipgloss.Height of the
// whole (borderless) group, and rows preceding g within its group via
// lipgloss.Height of each rendered row (the heading is a single line). Empty
// sections are skipped, mirroring renderBody exactly. Used to keep the
// selected row visible when scrolling the pipeline viewport.
func (m Model) orderedLineIndex(g int) int {
	inner := m.dims().pipeTextW

	line := 0
	seen := 0
	for _, s := range m.sectionList() {
		if len(s.rows) == 0 {
			continue
		}
		if g < seen+len(s.rows) {
			line++ // heading
			for i := 0; i < g-seen; i++ {
				line += lipgloss.Height(m.renderRow(s.rows[i], s, inner, 0))
			}
			return line
		}
		seen += len(s.rows)
		line += lipgloss.Height(m.renderSection(s, inner, 0))
	}
	return line
}
```

- [ ] Run to green: `go test ./cli/tui/ -race` — the new tests pass and the existing suite (`TestView_FillsFrameHeight`, `TestOrderedLineIndex_StrictlyIncreasing`, `TestMouseWheel_ScrollsPipeline`) stays green (the frame-height invariant holds because `pipeH + actH == bodyH` whenever `bodyH ≥ actMin + 4`, which every test size satisfies). Then `make lint`.

- [ ] Commit:

```
git add cli/tui/view.go cli/tui/sections.go cli/tui/layout_internal_test.go
git commit -m "feat(tui): content-size pipeline panel, drop empty section bands"
```

---

## Task 3 — P2: one ACTIVE section, honest header chips

Merge the RUNNING and IN FLIGHT sections into a single **ACTIVE** section: every row in a working column (`running`/`review`/`rework`/`merging`), live-claim rows first (spinner, active-lane badge, elapsed), then unclaimed rows dim with a `◇` lead. Header chips become `⚡ working · ◇ waiting · • queued · ✖ blocked · ✓ done`, where `working` counts live claims and `waiting` the unclaimed ACTIVE remainder — the chips and the section agree by construction.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `Model` fields `running`/`inFlight` (lines 100–103), accessors `Running`/`Blocked`/`Queued`/`InFlight` (lines 198–207), `fold` (lines 472–550).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — `section` struct (lines 78–87), `renderHeader`'s chips (lines 267–273), `inFlightCount` (lines 413–416).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sections.go` — `sectionList` (lines 14–21), `renderRow`'s lead/id cells (lines 74–90).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model_test.go` — the three tests that reference `Running()`/`InFlight()` (lines 63–108, 118–157, 167–208).

**Interfaces**
- Consumes: Task 1 palette vars; Task 2's empty-section skip (an empty ACTIVE band now simply vanishes).
- Produces: `func (m Model) Active() []Row` (replaces `Running()`/`InFlight()`); `func (m Model) waitingCount() int` (unexported); `section` gains `dimIdle bool`. Removes `Running()`, `InFlight()`, `inFlightCount()`. Later tasks (5, 6, 8) build on this fold shape and section list.

**Steps**

- [ ] Update the tests first (failing state). In `/Users/xlyk/Code/clipse/cli/tui/model_test.go`, replace `TestUpdate_SnapshotMsg_FoldsGroupsAndTotals` (lines 59–108) with:

```go
// TestUpdate_SnapshotMsg_FoldsGroupsAndTotals asserts that feeding a
// snapshotMsg into Update deterministically recomputes the
// active/blocked/queued groupings plus the token/count totals, with no DB
// access inside Update itself.
func TestUpdate_SnapshotMsg_FoldsGroupsAndTotals(t *testing.T) {
	m := tui.NewModel()
	snap := buildSnapshot()

	updated, cmd := m.Update(tui.SnapshotMsg{Snap: snap})
	if cmd != nil {
		t.Errorf("Update(snapshotMsg) cmd = %v, want nil (folding state should not itself schedule work)", cmd)
	}

	if got, want := len(updated.Active()), 1; got != want {
		t.Fatalf("Active() len = %d, want %d", got, want)
	}
	if got, want := updated.Active()[0].Identifier, "CLP-1"; got != want {
		t.Errorf("Active()[0].Identifier = %q, want %q", got, want)
	}

	if got, want := len(updated.Blocked()), 1; got != want {
		t.Fatalf("Blocked() len = %d, want %d", got, want)
	}
	if got, want := updated.Blocked()[0].Identifier, "CLP-2"; got != want {
		t.Errorf("Blocked()[0].Identifier = %q, want %q", got, want)
	}

	// QUEUED groups ready + todo together.
	queued := updated.Queued()
	if got, want := len(queued), 2; got != want {
		t.Fatalf("Queued() len = %d, want %d", got, want)
	}
	gotIDs := map[string]bool{queued[0].Identifier: true, queued[1].Identifier: true}
	for _, want := range []string{"CLP-3", "CLP-4"} {
		if !gotIDs[want] {
			t.Errorf("Queued() missing %q, got %+v", want, queued)
		}
	}

	if got, want := updated.TotalTokensIn(), 15; got != want {
		t.Errorf("TotalTokensIn() = %d, want %d", got, want)
	}
	if got, want := updated.TotalTokensOut(), 27; got != want {
		t.Errorf("TotalTokensOut() = %d, want %d", got, want)
	}

	if updated.Err() != nil {
		t.Errorf("Err() = %v, want nil after a clean snapshotMsg", updated.Err())
	}
}
```

- [ ] Replace `TestFold_DownstreamColumnsAppearInInFlightBucket` (lines 110–157) with:

```go
// TestFold_WorkingColumnsFoldIntoActive asserts every working column —
// running, review, rework, merging — lands in the single ACTIVE section
// (P2), while terminal "done" still shows up nowhere: this must stay an
// explicit set of active columns, not a catch-all default that would also
// sweep in "done".
func TestFold_WorkingColumnsFoldIntoActive(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "i-run", Identifier: "CLP-9", LaneLabel: "agent:coder", BoardStatus: "running"}},
			{Issue: store.Issue{ID: "i-review", Identifier: "CLP-10", LaneLabel: "agent:reviewer", BoardStatus: "review"}},
			{Issue: store.Issue{ID: "i-rework", Identifier: "CLP-11", LaneLabel: "agent:coder", BoardStatus: "rework"}},
			{Issue: store.Issue{ID: "i-merging", Identifier: "CLP-12", LaneLabel: "agent:git_operator", BoardStatus: "merging"}},
			{Issue: store.Issue{ID: "i-done", Identifier: "CLP-14", LaneLabel: "agent:coder", BoardStatus: "done"}},
		},
	}

	m := tui.NewModel()
	updated, _ := m.Update(tui.SnapshotMsg{Snap: snap})

	active := updated.Active()
	if got, want := len(active), 4; got != want {
		t.Fatalf("Active() len = %d, want %d (running/review/rework/merging); got %+v", got, want, active)
	}
	gotIDs := make(map[string]bool, len(active))
	for _, row := range active {
		gotIDs[row.Identifier] = true
	}
	for _, want := range []string{"CLP-9", "CLP-10", "CLP-11", "CLP-12"} {
		if !gotIDs[want] {
			t.Errorf("Active() missing %q, got %+v", want, active)
		}
	}
	if gotIDs["CLP-14"] {
		t.Errorf("done issue CLP-14 leaked into Active(), want it to stay invisible (terminal)")
	}

	if got := len(updated.Blocked()); got != 0 {
		t.Errorf("Blocked() len = %d, want 0", got)
	}
	if got := len(updated.Queued()); got != 0 {
		t.Errorf("Queued() len = %d, want 0", got)
	}
}

// TestFold_ActiveOrdersLiveFirst asserts the ACTIVE section leads with the
// rows a worker is on right now (held claim), then the unclaimed rows
// waiting for pickup — identifier order preserved within each half.
func TestFold_ActiveOrdersLiveFirst(t *testing.T) {
	claimed := sql.NullString{String: "claim-tok", Valid: true}
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			// Unclaimed running card (sorts first by identifier alone).
			{Issue: store.Issue{ID: "i-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "running"}},
			// Claimed review card — live, must lead despite the later identifier.
			{
				Issue:     store.Issue{ID: "i-2", Identifier: "CLP-2", LaneLabel: "coder", BoardStatus: "review", ClaimLock: claimed},
				LatestRun: &store.Run{RunID: "r2", Lane: "reviewer", Status: "running", StartedAt: 100},
			},
			// Unclaimed merging card.
			{Issue: store.Issue{ID: "i-3", Identifier: "CLP-3", LaneLabel: "coder", BoardStatus: "merging"}},
		},
	}

	m := tui.NewModel()
	updated, _ := m.Update(tui.SnapshotMsg{Snap: snap})

	active := updated.Active()
	if got, want := len(active), 3; got != want {
		t.Fatalf("Active() len = %d, want %d", got, want)
	}
	wantOrder := []string{"CLP-2", "CLP-1", "CLP-3"}
	for i, want := range wantOrder {
		if active[i].Identifier != want {
			t.Errorf("Active()[%d] = %q, want %q (live rows first, identifier order within halves)", i, active[i].Identifier, want)
		}
	}
}
```

- [ ] In `TestFold_ActiveClaimMarksRowLiveWithWorkingLane` (lines 159–208), replace the `byID` construction (lines 194–197):

```go
	byID := make(map[string]tui.Row)
	for _, r := range updated.Active() {
		byID[r.Identifier] = r
	}
```

(The three assertions below it are unchanged.)

- [ ] Run `go test ./cli/tui/ -race` — expect a **compile error** (`updated.Active` undefined). That is the failing state.

- [ ] Implement the model regrouping. In `/Users/xlyk/Code/clipse/cli/tui/model.go`:

Add `"sort"` to the imports:

```go
import (
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)
```

Replace the first four `Model` fields (lines 100–103, `running`/`blocked`/`queued`/`inFlight`) with:

```go
	// active is every issue in play right now — the working columns
	// running/review/rework/merging folded into one section (P2): live-claim
	// rows first (a worker is on them at this moment), then unclaimed rows
	// waiting for their next pickup.
	active  []Row
	blocked []Row
	queued  []Row // ready + todo, in that order
```

Update the `ordered` field's comment (line 105–107):

```go
	// ordered is every visible row flattened in section order
	// (active→blocked→queued), the list the selection cursor walks.
	ordered []Row
```

Replace the accessor block (lines 198–207, `// Running, Blocked, Queued, and InFlight expose …` through `func (m Model) InFlight() []Row { return m.inFlight }`) with:

```go
// Active, Blocked, and Queued expose the current display-ready rows for each
// dashboard section. Active folds every working column
// (running/review/rework/merging) together — live-claim rows first, then
// unclaimed rows awaiting pickup — labeled per-row by its own column
// (Row.Status) since it spans more than one board_status value. Queued folds
// "ready" and "todo" issues together.
func (m Model) Active() []Row  { return m.active }
func (m Model) Blocked() []Row { return m.blocked }
func (m Model) Queued() []Row  { return m.queued }
```

In `fold` (lines 472–550): replace the reset block (lines 473–477) with:

```go
	m.active = m.active[:0]
	m.blocked = m.blocked[:0]
	m.queued = m.queued[:0]
	m.ordered = m.ordered[:0]
```

Replace the `switch is.BoardStatus { … }` (lines 522–539) with:

```go
		switch is.BoardStatus {
		case "running", "review", "rework", "merging":
			// The working columns fold into one ACTIVE section (P2): a card
			// here is either currently claimed (a worker in some lane is on
			// it right now) or waiting its turn to be claimed — either way it
			// is in play. "done" deliberately has no case here (and this is
			// why the switch stays an explicit list rather than a catch-all
			// default): it's terminal, with nothing left to watch.
			m.active = append(m.active, row)
		case "blocked":
			m.blocked = append(m.blocked, row)
		case "ready", "todo":
			m.queued = append(m.queued, row)
		}
```

Replace the `ordered` concatenation block (lines 542–547) with:

```go
	// Live rows lead the ACTIVE section (spinner/lane/elapsed), unclaimed
	// rows trail dim; the stable sort preserves identifier order within each
	// half (rows arrive identifier-sorted from sortedIssueSnapshots).
	sort.SliceStable(m.active, func(i, j int) bool {
		return m.active[i].Live && !m.active[j].Live
	})

	// ordered is the section-order concatenation the cursor walks; it must
	// match View's stacked render order (active, blocked, queued).
	m.ordered = append(m.ordered, m.active...)
	m.ordered = append(m.ordered, m.blocked...)
	m.ordered = append(m.ordered, m.queued...)
```

- [ ] Implement the section list. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, add a field to the `section` struct (after `waiting bool`, line 86):

```go
	// dimIdle marks the ACTIVE section, whose unclaimed rows (no live
	// worker) render dim with a ◇ lead so the eye lands on the live rows
	// first.
	dimIdle bool
```

In `/Users/xlyk/Code/clipse/cli/tui/sections.go`, replace `sectionList` (lines 10–21) with:

```go
// sectionList returns the dashboard groups in render/navigation order.
// The order here MUST match fold's construction of m.ordered and
// orderedLineIndex's geometry, since the selection cursor walks them in
// lockstep.
func (m Model) sectionList() []section {
	return []section{
		{title: "ACTIVE", accent: cGreen, glyph: "⚡", rows: m.active, dimIdle: true},
		{title: "BLOCKED", accent: cRed, glyph: "✖", rows: m.blocked},
		{title: "QUEUED", accent: cAmber, glyph: "•", rows: m.queued, waiting: true},
	}
}
```

- [ ] Dim the idle ACTIVE rows. In `sections.go`'s `renderRow`, replace the lead-glyph and id-cell block (currently lines 77–90, from `// Liveness is per-row …` through `idCell = selIDStyle.Render(idText)` and its enclosing `if`) with:

```go
	// Liveness is per-row (an active claim = a worker on it now), so the
	// spinner lights up for a working agent in ANY lane — reviewer or
	// git_operator — not only the coder. An unclaimed ACTIVE row is parked
	// awaiting its next pickup: dim diamond, dim identifier (P2).
	lead := lipgloss.NewStyle().Foreground(s.accent).Render(s.glyph)
	switch {
	case row.Live:
		lead = lipgloss.NewStyle().Foreground(cGreen).Render(spinnerFrames[m.frame%len(spinnerFrames)])
	case s.dimIdle:
		lead = dimStyle.Render("◇")
	}

	idText := fmt.Sprintf("%-9s", row.Identifier)
	idCell := idStyle.Render(idText)
	if s.dimIdle && !row.Live {
		idCell = dimStyle.Render(idText)
	}
	if selected {
		idCell = selIDStyle.Render(idText)
	}
```

- [ ] Rework the header chips. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, replace the `chips := …` block inside `renderHeader` (lines 267–273) with:

```go
	chips := lipgloss.JoinHorizontal(lipgloss.Center,
		countChip("⚡", "working", m.workingCount(), cGreen), "   ",
		countChip("◇", "waiting", m.waitingCount(), cCyan), "   ",
		countChip("•", "queued", m.count("ready")+m.count("todo"), cAmber), "   ",
		countChip("✖", "blocked", m.count("blocked"), cRed), "   ",
		countChip("✓", "done", m.count("done"), cPurple),
	)
```

and replace `inFlightCount` (lines 413–416) with:

```go
// waitingCount is the number of ACTIVE rows no worker currently holds —
// cards parked in a working column (running/review/rework/merging) awaiting
// their next claim. Together with workingCount it partitions the ACTIVE
// section, so the header chips and the section rows agree by construction.
func (m Model) waitingCount() int {
	n := 0
	for _, r := range m.active {
		if !r.Live {
			n++
		}
	}
	return n
}
```

- [ ] Add the chips agreement test to `/Users/xlyk/Code/clipse/cli/tui/model_test.go` (append at the end of the file, before `assertErr`):

```go
// TestHeaderChips_WorkingWaitingAgreeWithActive asserts the header chips and
// the ACTIVE section agree by construction: one claimed row → "1 working",
// one unclaimed working-column row → "1 waiting". (Colors are stripped in a
// non-TTY test run, so the rendered strings are plain.)
func TestHeaderChips_WorkingWaitingAgreeWithActive(t *testing.T) {
	claimed := sql.NullString{String: "claim-tok", Valid: true}
	snap := store.Snapshot{
		CountsByStatus: map[string]int{"running": 1, "review": 1},
		Issues: []store.IssueSnapshot{
			{
				Issue:     store.Issue{ID: "i-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "running", ClaimLock: claimed},
				LatestRun: &store.Run{RunID: "r1", Lane: "coder", Status: "running", StartedAt: 100},
			},
			{Issue: store.Issue{ID: "i-2", Identifier: "CLP-2", LaneLabel: "coder", BoardStatus: "review"}},
		},
	}

	m := tui.NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(tui.SnapshotMsg{Snap: snap})

	view := m.View()
	if !strings.Contains(view, "1 working") {
		t.Errorf("View() missing %q chip", "1 working")
	}
	if !strings.Contains(view, "1 waiting") {
		t.Errorf("View() missing %q chip", "1 waiting")
	}
}
```

and add `"strings"` to `model_test.go`'s imports:

```go
import (
	"database/sql"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/cli/tui"
	"github.com/xlyk/clipse/internal/store"
)
```

- [ ] Run to green: `go test ./cli/tui/ -race` — all pass (note `TestSelectionNavigation_Clamps` still expects `CLP-1 → CLP-2 → CLP-3 → CLP-4`, which the new active→blocked→queued order preserves for `buildSnapshot`). Then `make lint`.

- [ ] Commit:

```
git add cli/tui/model.go cli/tui/view.go cli/tui/sections.go cli/tui/model_test.go
git commit -m "feat(tui): merge running and in-flight into one active section"
```

---

## Task 4 — P3: two-class activity feed, translated mechanics, lane dots

Split event kinds into **verdicts** (`merge`/`complete`/`blocked`/`request_changes`/`rework_cap_exceeded` — bold colored label, prose kept at full weight) and **mechanics** (`claimed`/`promoted`/`adopted`/`stale_release`/`retry_scheduled`/`orphan*`/`respawn` — the whole line dim). Translate kernel-speak (`stale_release` → "claim expired — requeued in <col>", `retry_scheduled` → "transient failure — retry <n>/<cap>: <reason>"), shorten every long hex id, and give each feed row a 1-cell lane dot resolved through the issue's runs.

The exact kernel strings being parsed (do not guess — these are pinned by the kernel today):
- `internal/store/claim.go` line 475: `released stale claim <32-hex-token> (column <from> -> <to>)`, kind `stale_release` (the event's `run_id` is the claim token, which equals the claiming run's id).
- `dispatcher/reconcile.go` line 207: `auto-retry <n>/<cap> after transient failure: <reason>`, kind `retry_scheduled`.
- `internal/store/claim.go` line 116: `claimed by run <run-id>`, kind `claimed`.
- Verdict kinds come from `internal/board/board.go` actions (`open_review`, `request_changes`, `merge`, `complete`, `comment_block`, `respawn`) plus dispatcher kinds `blocked`, `rework_cap_exceeded`, `retry_scheduled`, `orphaned`, `orphan_requeue`, `adopted`, `promoted`.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/activity.go` — `activityLines` (lines 18–59), `kindLabel` (lines 63–88), `cleanActivityDetail` (lines 94–101), `eventGlyph` (lines 106–123); new `classifyEvent`, `shortenHexIDs`, `laneDot`.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `Model` gains `laneByRunID map[string]string`; `fold` populates it.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/activity_internal_test.go` — update `TestKindLabel` / `TestCleanActivityDetail`, add classification/lane-dot tests.

**Interfaces**
- Consumes: Task 1's `eventGlyph` returning `lipgloss.AdaptiveColor`; `laneColor`/`bareLane` (view.go, unchanged); `shortID` (deps.go, unchanged).
- Produces: `type eventClass int` with `classNeutral`/`classVerdict`/`classMechanic`; `func classifyEvent(kind string) eventClass`; `func shortenHexIDs(s string) string`; `func (m Model) laneDot(e store.Event) string`; model field `laneByRunID`. Task 6 extends the same `fold` runs-loop this task introduces.

**Steps**

- [ ] Write the failing tests. In `/Users/xlyk/Code/clipse/cli/tui/activity_internal_test.go`, replace the whole file with:

```go
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
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestKindLabel|TestClassifyEvent|TestCleanActivityDetail|TestActivityLines_LaneDot'` — expect a **compile error** (`classifyEvent` undefined) plus label/translation failures once it compiles.

- [ ] Implement the model side. In `/Users/xlyk/Code/clipse/cli/tui/model.go`, add a field to `Model` directly after `statusByID map[string]string` (line 118):

```go
	// laneByRunID resolves a run id to its bare lane, built from every
	// issue's full run history — the activity feed uses it to badge each
	// event with the lane that produced it (P3).
	laneByRunID map[string]string
```

In `fold`, extend the map initialization block (after `m.statusByID = make(map[string]string, len(snap.Issues))`):

```go
	m.laneByRunID = make(map[string]string)
```

and inside the issue loop, directly after `m.statusByID[is.ID] = is.BoardStatus`:

```go
		for _, r := range is.Runs {
			m.laneByRunID[r.RunID] = r.Lane
		}
```

- [ ] Implement the feed. Replace `/Users/xlyk/Code/clipse/cli/tui/activity.go` in full with:

```go
package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/xlyk/clipse/internal/store"
)

// activityLines formats the recent-events feed (newest-first) into one aligned
// line each: "HH:MM:SS  <lane-dot> <id>  <glyph> <kind>  <detail>". Kinds are
// split into two classes (P3): verdicts — the board-moving outcomes — render a
// bold colored label with the prose detail kept at full text weight (it's the
// reviewer's voice); mechanics — kernel bookkeeping — render entirely dim with
// kernel-speak translated and long hex ids shortened. A pending refresh error
// is surfaced as the first line. Formatting a fixed event ts (time.Unix) is
// deterministic — not a wall-clock read — so this is safe from layout().
func (m Model) activityLines(width int) []string {
	var lines []string

	if m.lastErr != nil {
		lines = append(lines, lipgloss.NewStyle().Foreground(cRed).Render("⚠ ")+
			dimStyle.Render(truncatePlain("refresh error: "+oneLine(m.lastErr.Error()), maxInt(width-3, 6))))
	}

	if len(m.recentEvents) == 0 {
		if len(lines) == 0 {
			lines = append(lines, dimStyle.Render("no activity yet"))
		}
		return lines
	}

	// Fixed lead columns: ts(8) + gap(2) + lane dot(1) + gap(1) + id(7) +
	// gap(2) + glyph(1) + gap(1) + label(11) + gap(2). The detail fills
	// whatever remains.
	const lead = 8 + 2 + 1 + 1 + 7 + 2 + 1 + 1 + 11 + 2

	for _, e := range m.recentEvents {
		ts := dimStyle.Render(time.Unix(e.Ts, 0).Format("15:04:05"))

		ident := "—"
		if e.IssueID.Valid && e.IssueID.String != "" {
			if id := m.identByID[e.IssueID.String]; id != "" {
				ident = id
			} else {
				ident = shortID(e.IssueID.String)
			}
		}
		identText := fmt.Sprintf("%-7s", truncatePlain(ident, 7))

		class := classifyEvent(e.Kind)
		glyph, color := eventGlyph(e.Kind)
		label := fmt.Sprintf("%-11s", kindLabel(e.Kind))

		idCell := lipgloss.NewStyle().Foreground(cText).Render(identText)
		kindCell := lipgloss.NewStyle().Foreground(color).Render(glyph + " " + label)
		detailStyle := dimStyle
		switch class {
		case classVerdict:
			// Verdicts are the only loud feed lines: bold label, prose kept
			// at full text weight.
			kindCell = lipgloss.NewStyle().Foreground(color).Bold(true).Render(glyph + " " + label)
			detailStyle = lipgloss.NewStyle().Foreground(cText)
		case classMechanic:
			// Mechanics are bookkeeping: the whole line goes quiet.
			idCell = dimStyle.Render(identText)
			kindCell = dimStyle.Render(glyph + " " + label)
		}

		detail := truncatePlain(cleanActivityDetail(e.Kind, e.Detail), maxInt(width-lead, 6))

		lines = append(lines, ts+"  "+m.laneDot(e)+" "+idCell+"  "+kindCell+"  "+detailStyle.Render(detail))
	}
	return lines
}

// laneDot renders a 1-cell lane marker for a feed row, colored by the lane
// of the run the event references (lane color is identity — the same
// cyan/purple/orange code the row badges use). An event with no resolvable
// run lane gets a dim placeholder dot.
func (m Model) laneDot(e store.Event) string {
	if e.RunID.Valid {
		if lane := m.laneByRunID[e.RunID.String]; lane != "" {
			return lipgloss.NewStyle().Foreground(laneColor(bareLane(lane))).Render("●")
		}
	}
	return dimStyle.Render("·")
}

// eventClass buckets an event kind for the feed's two-class treatment (P3):
// verdicts are the board-moving outcomes and the only loud lines; mechanics
// are kernel bookkeeping and always render dim; everything else (open_review
// hand-offs, unknown kinds) keeps the neutral middle weight.
type eventClass int

const (
	classNeutral eventClass = iota
	classVerdict
	classMechanic
)

// classifyEvent maps an event kind to its class. Matching is substring-based
// (like eventGlyph/kindLabel) so kind variants — auto_merged, comment_block,
// orphan_requeue — land in the right bucket without an exhaustive list.
func classifyEvent(kind string) eventClass {
	switch {
	case strings.Contains(kind, "merge"),
		kind == "done",
		kind == "complete",
		strings.Contains(kind, "block"),
		strings.Contains(kind, "request"),
		strings.Contains(kind, "changes"),
		strings.Contains(kind, "cap"):
		return classVerdict
	case strings.Contains(kind, "claim"),
		kind == "promoted",
		kind == "adopted",
		strings.Contains(kind, "stale"),
		strings.Contains(kind, "release"),
		kind == "retry_scheduled",
		strings.Contains(kind, "orphan"),
		kind == "respawn":
		return classMechanic
	default:
		return classNeutral
	}
}

// kindLabel maps a raw event kind to a short, human label that fits the feed's
// fixed kind column without truncating mid-word. Order matters: the stale
// check must precede the claim check so "stale_release" reads "requeued", and
// the cap check must precede "request"/"changes" ones staying as-is.
func kindLabel(kind string) string {
	switch {
	case strings.Contains(kind, "merge"):
		return "merged"
	case kind == "done" || kind == "complete":
		return "complete"
	case kind == "promoted":
		return "promoted"
	case strings.Contains(kind, "cap"):
		return "rework cap"
	case strings.Contains(kind, "request") || strings.Contains(kind, "changes"):
		return "changes req"
	case strings.Contains(kind, "review"):
		return "review"
	case strings.Contains(kind, "stale") || strings.Contains(kind, "release"):
		return "requeued"
	case kind == "retry_scheduled":
		return "retry"
	case strings.Contains(kind, "orphan"):
		return "orphaned"
	case strings.Contains(kind, "claim"):
		return "claimed"
	case strings.Contains(kind, "block"):
		return "blocked"
	default:
		return truncatePlain(strings.ReplaceAll(kind, "_", " "), 11)
	}
}

var (
	// staleColRe pulls the requeue target out of store.ReleaseStaleClaims's
	// detail: "released stale claim <token> (column <from> -> <to>)".
	staleColRe = regexp.MustCompile(`\(column \S+ -> (\S+)\)`)
	// retryRe pulls attempt/cap/reason out of dispatcher's retry detail:
	// "auto-retry <n>/<cap> after transient failure: <reason>".
	retryRe = regexp.MustCompile(`^auto-retry (\d+)/(\d+) after transient failure: (.*)$`)
	// hexIDRe matches the long hex identifiers (claim tokens, run/issue ids)
	// that leak into event details.
	hexIDRe = regexp.MustCompile(`[0-9a-f]{16,}`)
)

// cleanActivityDetail collapses an event detail to one tidy line and
// translates kernel-speak into operator language (P3): a "claimed" detail is
// reduced to its short run id; a "stale_release" reads "claim expired —
// requeued in <col>" instead of a 32-char claim token and a "merging ->
// merging" arrow; a "retry_scheduled" reads "transient failure — retry
// <n>/<cap>: <reason>"; every remaining long hex id is shortened. An
// unparseable detail degrades to the flattened original — translation must
// never hide an event.
func cleanActivityDetail(kind, detail string) string {
	d := oneLine(detail)
	switch {
	case strings.Contains(kind, "claim"):
		d = strings.TrimPrefix(d, "claimed by run ")
		return "run " + shortID(d)
	case strings.Contains(kind, "stale"):
		if m := staleColRe.FindStringSubmatch(d); m != nil {
			return "claim expired — requeued in " + m[1]
		}
		return "claim expired — requeued"
	case kind == "retry_scheduled":
		if m := retryRe.FindStringSubmatch(d); m != nil {
			return fmt.Sprintf("transient failure — retry %s/%s: %s", m[1], m[2], m[3])
		}
		return shortenHexIDs(d)
	default:
		return shortenHexIDs(d)
	}
}

// shortenHexIDs truncates every long hex id in s to its first 8 chars, the
// same display form shortID gives a bare id.
func shortenHexIDs(s string) string {
	return hexIDRe.ReplaceAllStringFunc(s, shortID)
}

// eventGlyph maps an event kind to a leading glyph and color: merges/dones are
// green ✓, blocks are red ✖, claims cyan ▶, reviews cyan ◆, rework/changes
// amber ⟳, requeue/retry/orphan mechanics a dim ·, and everything else a dim ·.
func eventGlyph(kind string) (string, lipgloss.AdaptiveColor) {
	switch {
	case strings.Contains(kind, "merge") || kind == "done" || kind == "complete" || kind == "promoted":
		return "✓", cGreen
	case strings.Contains(kind, "block"):
		return "✖", cRed
	case strings.Contains(kind, "claim"):
		return "▶", cCyan
	case strings.Contains(kind, "request") || strings.Contains(kind, "changes") || strings.Contains(kind, "cap"):
		return "⟳", cAmber
	case strings.Contains(kind, "review"):
		return "◆", cCyan
	case strings.Contains(kind, "stale") || strings.Contains(kind, "release") || kind == "retry_scheduled" || strings.Contains(kind, "orphan"):
		return "·", cDim
	default:
		return "·", cDim
	}
}

// oneLine collapses a possibly multi-line event detail into a single trimmed
// line (event details can carry embedded worker output with newlines).
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
```

- [ ] Run to green: `go test ./cli/tui/ -race`. Then `make lint`.

- [ ] Commit:

```
git add cli/tui/activity.go cli/tui/model.go cli/tui/activity_internal_test.go
git commit -m "feat(tui): two-class activity feed with translated mechanics and lane dots"
```

---

## Task 5 — P4: render the dropped snapshot fields

The store already delivers seven fields the TUI never draws (all on `store.Issue` / `store.IssueSnapshot` in `/Users/xlyk/Code/clipse/internal/store/types.go`): `ReworkCount` (line 28), `RecoverAttempts` (line 41), `BlockedUntil` (line 51), `Priority` (line 54), `IssueSnapshot.Unmirrored` (line 133), `Snapshot.UnmirroredCount` (line 149), and `Run.HeartbeatAt` (line 72, already reachable via `Row.Run`). Render: a `⟳ ×<rework> r<recover>` chip, a `retry in <n>s` backoff countdown, a `⇅ linear pending` badge (+ header count), a `♥ <age>` stale-heartbeat warning on live rows, and priority as the QUEUED sort key. (`Description` is deferred to detail v2 per the spec.)

Two kernel facts the rendering encodes:
- `blocked_until` is set on the **re-queued** card (release column: ready/review/rework/merging), never on the parked `blocked` column — so the countdown renders on any non-live row inside its backoff window, not only BLOCKED-section rows.
- The dispatcher heartbeats every held claim once per tick (`dispatcher/reconcile.go` line 27; tick cadence = `cfg.PollIntervalS`, default 30s per `internal/config/config.go` line 13), so ~2× that (60s) with no heartbeat means a wedged worker or stopped dispatcher.
- QUEUED priority order mirrors `store.selectClaimCandidate`'s `ORDER BY` (`internal/store/claim.go` lines 62–66): `priority` 0 ("none") sorts last, 1 (urgent) … 4 (low) ascending, ties by identifier.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `Row` struct (lines 56–93), `fold`'s `Row` literal (lines 504–515), new model field `unmirroredCount`.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sections.go` — `rowDetail` (lines 117–149).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — `renderHeader` (unmirrored chip), new `staleHeartbeatS` const.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sort.go` — new `queuedRank`.
- Create: `/Users/xlyk/Code/clipse/cli/tui/sections_internal_test.go`.

**Interfaces**
- Consumes: Task 3's fold shape (`m.active`/`m.blocked`/`m.queued`, `section{dimIdle}`); Task 1 palette.
- Produces: `Row` fields `ReworkCount, RecoverAttempts, BlockedUntil, Priority int` / `Unmirrored bool`; `func queuedRank(p int) int`; `const staleHeartbeatS = 60`; model field `unmirroredCount int`. Task 6 and Task 8 leave these untouched.

**Steps**

- [ ] Write the failing tests. Create `/Users/xlyk/Code/clipse/cli/tui/sections_internal_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)

// TestRowDetail_ReworkAndRecoverChip asserts the ⟳ chip renders the rework
// count and rides the recover-attempts counter on the same chip (P4):
// ⟳ ×<rework> r<recover>, each part present only when > 0.
func TestRowDetail_ReworkAndRecoverChip(t *testing.T) {
	m := NewModel()
	tests := []struct {
		name string
		row  Row
		want []string
		not  []string
	}{
		{"rework only", Row{Identifier: "CLI-1", Status: "rework", ReworkCount: 1}, []string{"⟳ ×1"}, []string{"r1"}},
		{"recover only", Row{Identifier: "CLI-2", Status: "ready", RecoverAttempts: 2}, []string{"⟳ r2"}, []string{"×"}},
		{"both", Row{Identifier: "CLI-3", Status: "rework", ReworkCount: 2, RecoverAttempts: 1}, []string{"⟳ ×2 r1"}, nil},
		{"neither", Row{Identifier: "CLI-4", Status: "ready"}, nil, []string{"⟳"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.rowDetail(tt.row, section{}, 1000)
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("rowDetail = %q, want it to contain %q", got, w)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(got, n) {
					t.Errorf("rowDetail = %q, want it NOT to contain %q", got, n)
				}
			}
		})
	}
}

// TestRowDetail_RetryCountdown asserts a non-live row inside its
// retry-backoff window shows when it becomes claimable again, and that the
// countdown disappears once the window passes (P4). blocked_until is set on
// the re-queued card (its release column), so this must not be gated on the
// BLOCKED section.
func TestRowDetail_RetryCountdown(t *testing.T) {
	m := NewModel()
	row := Row{Identifier: "CLI-1", Status: "ready", BlockedUntil: 1040}

	if got := m.rowDetail(row, section{}, 1000); !strings.Contains(got, "retry in 40s") {
		t.Errorf("rowDetail inside backoff window = %q, want %q", got, "retry in 40s")
	}
	if got := m.rowDetail(row, section{}, 2000); strings.Contains(got, "retry in") {
		t.Errorf("rowDetail after backoff window = %q, want no countdown", got)
	}
}

// TestRowDetail_UnmirroredBadge asserts a row with a pending Linear outbox
// write carries the ⇅ badge (P4) — the outbox backlog visible where the
// operator looks.
func TestRowDetail_UnmirroredBadge(t *testing.T) {
	m := NewModel()
	row := Row{Identifier: "CLI-1", Status: "review", Unmirrored: true}
	if got := m.rowDetail(row, section{}, 1000); !strings.Contains(got, "⇅ linear pending") {
		t.Errorf("rowDetail = %q, want it to contain %q", got, "⇅ linear pending")
	}
}

// TestRowDetail_StaleHeartbeat asserts a live row whose run's heartbeat has
// gone quiet past staleHeartbeatS shows the ♥ warning, and a fresh heartbeat
// doesn't (P4).
func TestRowDetail_StaleHeartbeat(t *testing.T) {
	m := NewModel()
	stale := Row{
		Identifier: "CLI-1", Status: "running", Live: true,
		Run: &store.Run{RunID: "r1", Lane: "coder", StartedAt: 900, HeartbeatAt: 900},
	}
	if got := m.rowDetail(stale, section{}, 1000); !strings.Contains(got, "♥") {
		t.Errorf("rowDetail with %ds-old heartbeat = %q, want a ♥ warning", 100, got)
	}
	fresh := Row{
		Identifier: "CLI-2", Status: "running", Live: true,
		Run: &store.Run{RunID: "r2", Lane: "coder", StartedAt: 900, HeartbeatAt: 990},
	}
	if got := m.rowDetail(fresh, section{}, 1000); strings.Contains(got, "♥") {
		t.Errorf("rowDetail with fresh heartbeat = %q, want no ♥ warning", got)
	}
}

// TestFold_QueuedSortsByPriority asserts QUEUED orders by the kernel's own
// claim order (P4, mirroring store.selectClaimCandidate): priority 1 (urgent)
// first, 0 ("none") last, ties by identifier.
func TestFold_QueuedSortsByPriority(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "a", Identifier: "CLI-1", BoardStatus: "todo", Priority: 0}},
			{Issue: store.Issue{ID: "b", Identifier: "CLI-2", BoardStatus: "todo", Priority: 4}},
			{Issue: store.Issue{ID: "c", Identifier: "CLI-3", BoardStatus: "ready", Priority: 1}},
			{Issue: store.Issue{ID: "d", Identifier: "CLI-4", BoardStatus: "todo", Priority: 1}},
		},
	}
	m := NewModel()
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	want := []string{"CLI-3", "CLI-4", "CLI-2", "CLI-1"}
	queued := m.Queued()
	if len(queued) != len(want) {
		t.Fatalf("Queued() len = %d, want %d", len(queued), len(want))
	}
	for i, w := range want {
		if queued[i].Identifier != w {
			t.Errorf("Queued()[%d] = %q, want %q (priority order: 1,1,4,none; ties by identifier)", i, queued[i].Identifier, w)
		}
	}
}

// TestHeader_UnmirroredChip asserts the header grows an amber unmirrored
// count when the outbox has pending Linear writes, and stays silent at zero.
func TestHeader_UnmirroredChip(t *testing.T) {
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: store.Snapshot{UnmirroredCount: 3}})
	if view := m.View(); !strings.Contains(view, "3 unmirrored") {
		t.Errorf("View() missing %q chip", "3 unmirrored")
	}

	m, _ = m.Update(SnapshotMsg{Snap: store.Snapshot{}})
	if view := m.View(); strings.Contains(view, "unmirrored") {
		t.Errorf("View() shows an unmirrored chip at zero pending writes")
	}
}
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestRowDetail_|TestFold_QueuedSortsByPriority|TestHeader_UnmirroredChip'` — expect a **compile error** (`Row` has no field `ReworkCount`).

- [ ] Implement the `Row` fields. In `/Users/xlyk/Code/clipse/cli/tui/model.go`, add to the `Row` struct after the `ActiveLane string` field (line 92):

```go
	// ReworkCount / RecoverAttempts / BlockedUntil / Priority / Unmirrored
	// surface the kernel's per-issue bookkeeping the TUI previously dropped
	// (P4): how many times the card bounced back to the coder, how many
	// transient-failure auto-retries it has burned, the unix time before
	// which it is invisible to every claim (its retry backoff window), its
	// Linear priority (the kernel's claim-order key), and whether a board
	// transition is still waiting to be mirrored to Linear (a pending
	// outbox write).
	ReworkCount     int
	RecoverAttempts int
	BlockedUntil    int64
	Priority        int
	Unmirrored      bool
```

Add a model field after `counts map[string]int` (line 126):

```go
	// unmirroredCount is how many issues have a pending (unmirrored) Linear
	// outbox write — surfaced as an amber header chip when > 0.
	unmirroredCount int
```

In `fold`, after `m.doneCount = snap.CountsByStatus["done"]`:

```go
	m.unmirroredCount = snap.UnmirroredCount
```

and extend the `Row` literal (the `row := Row{ … }` inside the issue loop) to:

```go
		row := Row{
			ID:              is.ID,
			Identifier:      is.Identifier,
			LaneLabel:       is.LaneLabel,
			Status:          is.BoardStatus,
			Deps:            is.Deps,
			Run:             is.LatestRun,
			TokensIn:        is.TokensInTotal,
			TokensOut:       is.TokensOutTotal,
			Live:            live,
			ActiveLane:      activeLane,
			ReworkCount:     is.ReworkCount,
			RecoverAttempts: is.RecoverAttempts,
			BlockedUntil:    is.BlockedUntil,
			Priority:        is.Priority,
			Unmirrored:      is.Unmirrored,
		}
```

Then add the priority sort directly before the `sort.SliceStable(m.active, …)` added in Task 3:

```go
	// QUEUED sorts by claim priority — the order the kernel will actually
	// take them (store.selectClaimCandidate's ORDER BY): Linear priority 0
	// means "none" and sorts last, 1 (urgent) … 4 (low) ascending, ties by
	// identifier (rows arrive identifier-sorted; the stable sort keeps that).
	sort.SliceStable(m.queued, func(i, j int) bool {
		return queuedRank(m.queued[i].Priority) < queuedRank(m.queued[j].Priority)
	})
```

- [ ] Add `queuedRank`. In `/Users/xlyk/Code/clipse/cli/tui/sort.go`, add `"math"` to the imports and append:

```go
// queuedRank maps a Linear priority to its claim-order rank, mirroring
// store.selectClaimCandidate's ORDER BY (CASE priority WHEN 0 THEN <max>
// ELSE priority END ASC): 0 ("no priority") ranks last; 1 (urgent) through
// 4 (low) rank ascending.
func queuedRank(p int) int {
	if p == 0 {
		return math.MaxInt
	}
	return p
}
```

- [ ] Add the heartbeat threshold. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, after the palette var block:

```go
// staleHeartbeatS is the heartbeat-age threshold (seconds) past which a live
// row gets the ♥ warning. The dispatcher heartbeats every held claim once per
// tick (cfg.PollIntervalS, default 30s — see dispatcher.reconcile), so ~2×
// that with no heartbeat means the worker or the dispatcher is wedged.
const staleHeartbeatS = 60
```

- [ ] Implement the row rendering. In `/Users/xlyk/Code/clipse/cli/tui/sections.go`, replace `rowDetail` (lines 114–149 including its doc comment) with:

```go
// rowDetail renders the trailing metadata. For a QUEUED row with unmet
// dependencies it shows a "waiting on …" hint instead; otherwise it shows
// turn count, the rework/recover chip, cumulative tokens, a retry-backoff
// countdown, an outbox-pending badge, and — for a live row — elapsed runtime
// plus a stale-heartbeat warning (P4).
func (m Model) rowDetail(row Row, s section, now int64) string {
	if s.waiting {
		if unmet := unmetDeps(row.Deps, m.identByID, m.statusByID); len(unmet) > 0 {
			// Cap the listed deps so a heavily-blocked card's detail can't grow
			// wide enough to wrap the row (which would also throw off the body
			// line geometry orderedLineIndex measures).
			const maxShown = 3
			suffix := ""
			if len(unmet) > maxShown {
				suffix = fmt.Sprintf(" +%d", len(unmet)-maxShown)
				unmet = unmet[:maxShown]
			}
			return waitingStyle.Render("⏳ waiting on " + strings.Join(unmet, ", ") + suffix)
		}
	}

	var parts []string
	if row.Run != nil {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("turn %d", row.Run.TurnCount)))
	}
	if row.ReworkCount > 0 || row.RecoverAttempts > 0 {
		// ⟳ ×<rework> r<recover>: how many times this card bounced back to
		// the coder, and how many transient-failure auto-retries it has
		// burned. The caps live in clipse.yaml, which the TUI deliberately
		// doesn't load, so the counts render bare.
		chip := "⟳"
		if row.ReworkCount > 0 {
			chip += fmt.Sprintf(" ×%d", row.ReworkCount)
		}
		if row.RecoverAttempts > 0 {
			chip += fmt.Sprintf(" r%d", row.RecoverAttempts)
		}
		parts = append(parts, waitingStyle.Render(chip))
	}
	if row.TokensIn > 0 || row.TokensOut > 0 {
		parts = append(parts, lipgloss.NewStyle().Foreground(cCyan).Render("↓"+humanizeTokens(row.TokensIn))+
			dimStyle.Render(" ")+
			lipgloss.NewStyle().Foreground(cPurple).Render("↑"+humanizeTokens(row.TokensOut)))
	}
	if !row.Live && row.BlockedUntil > now {
		// The retry-backoff countdown: the kernel sets blocked_until on a
		// re-queued card (its release column), making it invisible to every
		// claim until the window passes — show when it becomes claimable.
		parts = append(parts, waitingStyle.Render("retry in "+formatAge(row.BlockedUntil-now)))
	}
	if row.Unmirrored {
		parts = append(parts, waitingStyle.Render("⇅ linear pending"))
	}
	if row.Live {
		parts = append(parts, lipgloss.NewStyle().Foreground(cGreen).Render("⏱ "+formatElapsed(row.Run, now)))
		if row.Run != nil && row.Run.HeartbeatAt > 0 && now-row.Run.HeartbeatAt > staleHeartbeatS {
			// The claim is heartbeated every dispatcher tick; this much
			// silence on a still-claimed row means a wedged worker or a
			// stopped dispatcher.
			parts = append(parts, waitingStyle.Render("♥ "+formatAge(now-row.Run.HeartbeatAt)))
		}
	}
	if len(parts) == 0 {
		return dimStyle.Render("—")
	}
	return strings.Join(parts, dimStyle.Render(" · "))
}
```

- [ ] Implement the header chip. In `/Users/xlyk/Code/clipse/cli/tui/view.go`'s `renderHeader`, directly after the `chips := lipgloss.JoinHorizontal( … )` block from Task 3, add:

```go
	if m.unmirroredCount > 0 {
		// The outbox is a kernel invariant; a pending backlog (Linear
		// unreachable) should be visible where the operator looks (P4).
		chips = lipgloss.JoinHorizontal(lipgloss.Center, chips, "   ",
			countChip("⇅", "unmirrored", m.unmirroredCount, cAmber))
	}
```

- [ ] Run to green: `go test ./cli/tui/ -race`. Then `make lint`. (Note for the countdown/heartbeat parts: `layout()` renders with `now=0`, where `BlockedUntil > 0` shows a stale countdown and `now-HeartbeatAt` goes negative — both are inline segments that never change line counts, the same determinism contract `formatElapsed` already relies on.)

- [ ] Commit:

```
git add cli/tui/model.go cli/tui/sections.go cli/tui/view.go cli/tui/sort.go cli/tui/sections_internal_test.go
git commit -m "feat(tui): render rework, retry backoff, unmirrored, heartbeat and priority"
```

---

## Task 6 — P6: small-fixes batch

Five independent nits: tab/help label consistency ("board" everywhere); kanban cards show the active lane + spinner when live; the DONE line gains PR numbers; the footer context shows the selected row's status; the cost estimate is labeled `est.` and priced with two per-lane rate classes (Sonnet-class coder lanes, Opus-class reviewer) instead of one.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/keys.go` — `Kanban` binding help (line 45).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/kanban.go` — `renderKanbanCard` (lines 129–139).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sections.go` — `renderDoneSummary` (lines 154–171), new `prNumber`.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — `renderFooter` (lines 378–385), `renderProgress` (lines 364–373).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/deps.go` — cost constants + `estimateCostUSD` (lines 82–93).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `Model` gains `laneTokens map[string][2]int`; `fold` populates it.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/deps_test.go` — `TestEstimateCostUSD` (lines 93–102).
- Modify: `/Users/xlyk/Code/clipse/cli/tui/sections_internal_test.go` — append kanban/done/footer tests.

**Interfaces**
- Consumes: Task 3's `Row.ActiveLane`/`Live` and `m.frame` spinner; Task 4's `fold` runs-loop (this task extends the same loop); `prURLFromRuns` + `parseResult` (detail.go, unchanged); `bareLane` (view.go).
- Produces: `func estimateCostUSD(laneTokens map[string][2]int) float64` (signature change), `func prNumber(url string) string`, model field `laneTokens map[string][2]int`.

**Steps**

- [ ] Write the failing tests. In `/Users/xlyk/Code/clipse/cli/tui/deps_test.go`, replace `TestEstimateCostUSD` (lines 93–102) with:

```go
// TestEstimateCostUSD asserts the two-rate-class display cost math (P6):
// reviewer tokens price at Opus-class rates, every other lane at
// Sonnet-class. Still an estimate — the honest fix (persisting runs.model)
// is deferred to U6 — but no longer silently 5× low on reviewer tokens.
func TestEstimateCostUSD(t *testing.T) {
	// 1M coder input @ $3 + 1M coder output @ $15 = $18.00.
	coderOnly := map[string][2]int{"coder": {1_000_000, 1_000_000}}
	if got := estimateCostUSD(coderOnly); math.Abs(got-18.0) > 1e-9 {
		t.Errorf("estimateCostUSD(coder 1M/1M) = %f, want 18.0", got)
	}
	// 1M reviewer input @ $15 + 1M reviewer output @ $75 = $90.00.
	reviewerOnly := map[string][2]int{"reviewer": {1_000_000, 1_000_000}}
	if got := estimateCostUSD(reviewerOnly); math.Abs(got-90.0) > 1e-9 {
		t.Errorf("estimateCostUSD(reviewer 1M/1M) = %f, want 90.0", got)
	}
	// The agent: prefix normalizes away, matching bareLane's contract.
	prefixed := map[string][2]int{"agent:reviewer": {1_000_000, 0}}
	if got := estimateCostUSD(prefixed); math.Abs(got-15.0) > 1e-9 {
		t.Errorf("estimateCostUSD(agent:reviewer 1M in) = %f, want 15.0", got)
	}
	if got := estimateCostUSD(nil); got != 0 {
		t.Errorf("estimateCostUSD(nil) = %f, want 0", got)
	}
}
```

- [ ] Append to `/Users/xlyk/Code/clipse/cli/tui/sections_internal_test.go`:

```go
// TestPRNumber asserts the trailing PR number extraction used by the DONE
// summary (P6): a GitHub PR URL yields "#<n>", anything else yields "".
func TestPRNumber(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://github.com/xlyk/demo/pull/38", "#38"},
		{"https://github.com/xlyk/demo/pull/38/", ""},
		{"https://github.com/xlyk/demo", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := prNumber(tt.url); got != tt.want {
			t.Errorf("prNumber(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}

// TestRenderDoneSummary_IncludesPRNumbers asserts the DONE line pairs each
// completed identifier with its merged PR number when a run carried one (P6).
func TestRenderDoneSummary_IncludesPRNumbers(t *testing.T) {
	pr := `{"outcome":"done","summary":"merged","pr_url":"https://github.com/xlyk/demo/pull/38"}`
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{
				Issue: store.Issue{ID: "i-1", Identifier: "CLI-52", LaneLabel: "coder", BoardStatus: "done"},
				Runs: []store.Run{
					{RunID: "r1", IssueID: "i-1", Lane: "coder", Status: "done", ResultJSON: sql.NullString{String: pr, Valid: true}},
				},
			},
		},
	}
	m := NewModel()
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	done := m.renderDoneSummary(100)
	if !strings.Contains(done, "CLI-52 #38") {
		t.Errorf("renderDoneSummary = %q, want it to contain %q", done, "CLI-52 #38")
	}
}

// TestKanbanCard_LiveShowsSpinnerAndActiveLane asserts a live kanban card
// animates (spinner glyph) and badges the lane actually working it, not the
// home label — so the board tab visibly runs (P6).
func TestKanbanCard_LiveShowsSpinnerAndActiveLane(t *testing.T) {
	m := NewModel()
	live := Row{Identifier: "CLI-1", LaneLabel: "coder", Status: "review", Live: true, ActiveLane: "reviewer"}
	card := m.renderKanbanCard(live)
	if !strings.Contains(card, "reviewer") {
		t.Errorf("live card = %q, want the active lane %q badged", card, "reviewer")
	}
	if !strings.Contains(card, spinnerFrames[0]) {
		t.Errorf("live card = %q, want spinner frame %q", card, spinnerFrames[0])
	}

	parked := Row{Identifier: "CLI-2", LaneLabel: "coder", Status: "review"}
	card = m.renderKanbanCard(parked)
	if strings.Contains(card, spinnerFrames[0]) {
		t.Errorf("parked card = %q, want no spinner", card)
	}
	if !strings.Contains(card, "coder") {
		t.Errorf("parked card = %q, want the home lane %q", card, "coder")
	}
}

// TestFooter_ShowsSelectedStatus asserts the footer context names the
// selected row's column: "dashboard · CLI-1 · review" (P6).
func TestFooter_ShowsSelectedStatus(t *testing.T) {
	snap := store.Snapshot{
		Issues: []store.IssueSnapshot{
			{Issue: store.Issue{ID: "i-1", Identifier: "CLI-1", LaneLabel: "coder", BoardStatus: "review"}},
		},
	}
	m := NewModel()
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m, _ = m.Update(SnapshotMsg{Snap: snap})

	footer := m.renderFooter(100)
	if !strings.Contains(footer, "CLI-1 · review") {
		t.Errorf("renderFooter = %q, want it to contain %q", footer, "CLI-1 · review")
	}
}
```

and add `"database/sql"` to `sections_internal_test.go`'s imports:

```go
import (
	"database/sql"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/xlyk/clipse/internal/store"
)
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestEstimateCostUSD|TestPRNumber|TestRenderDoneSummary_IncludesPRNumbers|TestKanbanCard_|TestFooter_'` — expect a **compile error** (`estimateCostUSD` takes two ints; `prNumber` undefined).

- [ ] Implement the cost model. In `/Users/xlyk/Code/clipse/cli/tui/deps.go`, replace the cost block (lines 81–93, from `// Rough, display-only blended token prices …` to the end of `estimateCostUSD`) with:

```go
// Rough, display-only per-lane token prices ($/token). The coder lanes run a
// Sonnet-class model and the reviewer an Opus-class one by default (see
// AGENTS.md "Model config"); pricing the two classes separately keeps the
// header estimate within the right order of magnitude instead of silently
// ~5× low on reviewer tokens. Still NOT billing-accurate (the honest fix —
// persisting runs.model — is deferred to U6), which is why the header labels
// it "est.". git_operator is deterministic Go and records no tokens.
const (
	sonnetInPerTok  = 3.0 / 1_000_000  // ~$3 per 1M input tokens
	sonnetOutPerTok = 15.0 / 1_000_000 // ~$15 per 1M output tokens
	opusInPerTok    = 15.0 / 1_000_000 // ~$15 per 1M input tokens
	opusOutPerTok   = 75.0 / 1_000_000 // ~$75 per 1M output tokens
)

// estimateCostUSD prices per-lane cumulative token sums ({in, out} pairs
// keyed by lane) with the two display rate classes: reviewer tokens at
// Opus-class rates, every other lane at Sonnet-class.
func estimateCostUSD(laneTokens map[string][2]int) float64 {
	total := 0.0
	for lane, t := range laneTokens {
		in, out := float64(t[0]), float64(t[1])
		if bareLane(lane) == "reviewer" {
			total += in*opusInPerTok + out*opusOutPerTok
		} else {
			total += in*sonnetInPerTok + out*sonnetOutPerTok
		}
	}
	return total
}
```

- [ ] Feed it per-lane sums. In `/Users/xlyk/Code/clipse/cli/tui/model.go`, add a field after the `laneByRunID` field added in Task 4:

```go
	// laneTokens sums cumulative token usage ({in, out}) per lane across
	// every run of every issue, for the header's two-rate cost estimate.
	laneTokens map[string][2]int
```

In `fold`, next to `m.laneByRunID = make(map[string]string)`:

```go
	m.laneTokens = make(map[string][2]int)
```

and extend the per-issue runs loop (from Task 4) to:

```go
		for _, r := range is.Runs {
			m.laneByRunID[r.RunID] = r.Lane
			t := m.laneTokens[r.Lane]
			t[0] += r.TokensIn
			t[1] += r.TokensOut
			m.laneTokens[r.Lane] = t
		}
```

- [ ] Update the header cost line. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, replace the `cost := …` line inside `renderProgress` (line 371):

```go
	cost := costStyle.Render(fmt.Sprintf("   est. $%.2f", estimateCostUSD(m.laneTokens)))
```

- [ ] Footer status. In `view.go`, replace `renderFooter` (lines 375–385 including doc comment) with:

```go
// renderFooter draws the pinned bottom bar: a thin full-width rule, then the
// key hints (bubbles help, short form) on the left with a "mode · selection ·
// status" context flush right.
func (m Model) renderFooter(cw int) string {
	ctx := m.ViewMode()
	if m.selected != "" {
		ctx += " · " + m.selected
		if is, ok := m.issuesByIdent[m.selected]; ok {
			ctx += " · " + is.BoardStatus
		}
	}
	line := padBetween(footerStyle.Render(m.help.View(m.keys)), dimStyle.Render(ctx), cw)
	return lipgloss.JoinVertical(lipgloss.Left, ruleStyle.Render(strings.Repeat("─", cw)), line)
}
```

- [ ] Kanban cards. In `/Users/xlyk/Code/clipse/cli/tui/kanban.go`, replace `renderKanbanCard` (lines 125–139 including doc comment) with:

```go
// renderKanbanCard renders one issue as a compact card: identifier over a
// lane badge. A live card (held claim) animates a spinner and badges the
// lane actually working it (ActiveLane) rather than the home label, so the
// board tab visibly runs mid-pipeline (P6). The selected card is marked and
// its identifier reversed. The enclosing column box (Width(colW)) bounds the
// card width, so no manual clipping is needed here.
func (m Model) renderKanbanCard(row Row) string {
	selected := row.Identifier == m.selected

	id := idStyle.Render(row.Identifier)
	prefix := "  "
	if selected {
		id = selIDStyle.Render(row.Identifier)
		prefix = selMarkStyle.Render("▌") + " "
	}

	badgeLane := row.LaneLabel
	lead := "  "
	if row.Live {
		if row.ActiveLane != "" {
			badgeLane = row.ActiveLane
		}
		lead = lipgloss.NewStyle().Foreground(cGreen).Render(spinnerFrames[m.frame%len(spinnerFrames)]) + " "
	}
	return lipgloss.JoinVertical(lipgloss.Left, prefix+id, lead+laneBadge(badgeLane))
}
```

- [ ] DONE line PR numbers. In `/Users/xlyk/Code/clipse/cli/tui/sections.go`, replace `renderDoneSummary` (lines 151–171 including doc comment) with, and append `prNumber` after it:

```go
// renderDoneSummary renders a single compact line listing completed issues
// (dim) with their merged PR numbers when a run carried one — "CLI-52 #38" —
// so terminal "done" cards remain visible and satisfying (P6). Returns ""
// when nothing is done.
func (m Model) renderDoneSummary(inner int) string {
	done := m.byStatus["done"]
	if len(done) == 0 {
		return ""
	}
	idents := make([]string, 0, len(done))
	for _, r := range done {
		label := r.Identifier
		if is, ok := m.issuesByIdent[r.Identifier]; ok {
			if n := prNumber(prURLFromRuns(is.Runs)); n != "" {
				label += " " + n
			}
		}
		idents = append(idents, label)
	}
	// Match the section groups: a full-width labeled band, then the completed
	// identifiers on the line below (budgeted so the line never wraps).
	head := doneHeadStyle.Render("✓ DONE") + dimStyle.Render(fmt.Sprintf(" (%d)", len(done)))
	if fill := inner - lipgloss.Width(head) - 1; fill > 0 {
		head += " " + ruleStyle.Render(strings.Repeat("─", fill))
	}
	list := dimStyle.Render("   " + truncatePlain(strings.Join(idents, "  "), maxInt(inner-4, 4)))
	return head + "\n" + list
}

// prNumber extracts a "#<digits>" display form from a PR URL's trailing path
// segment ("…/pull/38" → "#38"), or "" when the URL doesn't end in a number.
func prNumber(url string) string {
	i := strings.LastIndex(url, "/")
	if i < 0 || i == len(url)-1 {
		return ""
	}
	tail := url[i+1:]
	for _, r := range tail {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "#" + tail
}
```

- [ ] Label consistency. In `/Users/xlyk/Code/clipse/cli/tui/keys.go`, the `Kanban` binding (line 43–46) — the tab bar says "board", so the help hint must too:

```go
		Kanban: key.NewBinding(
			key.WithKeys("tab", "v"),
			key.WithHelp("tab", "board"),
		),
```

- [ ] Run to green: `go test ./cli/tui/ -race`. Then `make lint`.

- [ ] Commit:

```
git add cli/tui/keys.go cli/tui/kanban.go cli/tui/sections.go cli/tui/view.go cli/tui/deps.go cli/tui/model.go cli/tui/deps_test.go cli/tui/sections_internal_test.go
git commit -m "fix(tui): board label, live kanban cards, done pr numbers, per-lane est cost"
```

---

## Task 7 — U1 (part 1): follow-mode tailer — offset handling, JSONL parse, transcript renderer

The pure core of follow mode, plus its one I/O `tea.Cmd`, in a new `follow.go` — no model/key/view wiring yet (Task 8). Ground truth for what it parses:

- **Transcript** `<board>/logs/<ISSUE>.transcript.jsonl` (path built by `dispatcher.transcriptPath`, `dispatcher/spawn.go` line 178, keyed by issue identifier). Append-only JSONL, one JSON object per line, written by `agent/src/clipse_agent/transcript.py`. Every line carries the bind context `lane`, `run_id`, `thread_id`, `assistant_id`, `model` plus a float `ts` (epoch seconds), and an `event` discriminator with per-type fields (emitted in `agent/src/clipse_agent/dac.py`):
  - `turn_start`: `task_text`
  - `assistant`: `text`
  - `tool_call`: `name`, `args` (object; shell commands carry `args.command`)
  - `tool_result`: `name`, `status`, `content`
  - `interrupt`: `payload` (a repr string)
  - `turn_end`: `outcome_hint`, `tokens_in`, `tokens_out` — or, on a crashed turn, just `error`
  - There is **no duration field**; a tool call's duration is derived from the `ts` delta between the call and its matching result.
- **Raw stderr** `<board>/logs/<ISSUE>.log` (path built by `spawn.LocalSpawner.stderrLogPath`, `internal/spawn/local.go` line 171, also keyed by identifier). Opened with `os.O_CREATE|os.O_WRONLY|os.O_TRUNC` on **every spawn** (`local.go` line 107) — so a file whose size shrank below the stored offset means "new run started: reset offset to 0 and re-read". The transcript, by contrast, is append-only forever and only hits the same reset path if a human deletes it.

Mandated tailer semantics (from the spec): poll the active file every ~500ms from a stored byte offset via a `tea.Cmd` (I/O never in `Update`); shrink ⇒ offset reset to 0; unknown/malformed lines render dim raw and never crash.

**Files**
- Create: `/Users/xlyk/Code/clipse/cli/tui/follow.go`
- Create: `/Users/xlyk/Code/clipse/cli/tui/follow_test.go` (internal, `package tui`)

**Interfaces produced** (Task 8 consumes all of these exactly as named):
- `type followSource int` with `followTranscript` / `followRaw`
- `type followState struct { ident string; source followSource; offset int64; partial []byte; events []transcriptEvent; rawLines []string; pinned bool; err error }` with methods `applyChunk(data []byte, newOffset int64, reset bool)` and `lane() string`
- `type transcriptEvent struct` (JSON-tagged fields listed below)
- `func parseTranscriptLine(line string) transcriptEvent`
- `func renderTranscriptLines(events []transcriptEvent, width int) []string`
- `func renderRawLines(lines []string, width int) []string`
- `type followPollMsg struct { Path string; Data []byte; Offset int64; Reset bool; NotExist bool; Err error }`
- `type followTickMsg struct{}`
- `const followInterval = 500 * time.Millisecond`
- `func scheduleFollowTick() tea.Cmd`
- `func pollFollowFile(path string, offset int64) tea.Cmd`

**Steps**

- [ ] Write the failing tests. Create `/Users/xlyk/Code/clipse/cli/tui/follow_test.go`:

```go
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
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestParseTranscriptLine|TestFollowState_|TestRenderTranscriptLines|TestPollFollowFile'` — expect a **compile error** (nothing in `follow.go` exists yet).

- [ ] Implement. Create `/Users/xlyk/Code/clipse/cli/tui/follow.go`:

```go
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
// most once). It returns the outcome per call event index and the set of
// result event indices that were consumed — an unconsumed result (its call
// scrolled away in a reset) still renders standalone.
func matchToolResults(events []transcriptEvent) (map[int]toolOutcome, map[int]bool) {
	outcomes := make(map[int]toolOutcome)
	consumed := make(map[int]bool)
	open := make(map[string][]int)
	for i, ev := range events {
		switch ev.Event {
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
```

- [ ] Run to green: `go test ./cli/tui/ -race`. Then `make lint`. (The duration assertion `→ success · 2.1s` comes from `103.1 − 101 = 2.1`; `%.1fs` renders `2.1s`.)

- [ ] Commit:

```
git add cli/tui/follow.go cli/tui/follow_test.go
git commit -m "feat(tui): follow-mode tailer, transcript parser and renderer"
```

---

## Task 8 — U1 (part 2): follow-mode UI — `f` tails a live worker, `t` toggles raw stderr, `esc` back

Wire Task 7's tailer into the model: a `modeFollow` view, the `f`/`t` keys, the 500ms tick loop, bottom-pinned auto-scroll, and the `boardDir`-derived paths from `runTUI`. Gating rule (spec): `f` works only on a row with a live claim **or** an existing transcript file. File existence is I/O, so it cannot be checked in `Update`: the refresh command in `cli/tui.go` (already an I/O closure) lists the logs dir once per 2s poll and ships the set of identifiers with transcripts on the `SnapshotMsg`; `fold` stamps it onto each `Row`.

**Files**
- Modify: `/Users/xlyk/Code/clipse/cli/tui/model.go` — `modeFollow` const, `SnapshotMsg.TranscriptIdents`, `Model` fields (`logsDir`, `transcripts`, `follow`, `followVp`), `WithLogsDir`, `Row.HasTranscript`, `Update` cases, `handleKey`, `handleMouse`, `scrollActive`, `ViewMode`, `layout`.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/keys.go` — `Follow` / `ToggleSource` bindings.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/view.go` — `View` dispatch, new `renderFollowScreen`.
- Modify: `/Users/xlyk/Code/clipse/cli/tui.go` — `runTUI` wiring (`WithLogsDir`, `TranscriptIdents`), new `transcriptIdents` helper.
- Modify: `/Users/xlyk/Code/clipse/cli/tui/follow_test.go` — append the UI tests.

**Interfaces**
- Consumes everything Task 7 produced (`followState`, `pollFollowFile`, `scheduleFollowTick`, `followPollMsg`, `followTickMsg`, `renderTranscriptLines`, `renderRawLines`, `followTranscript`/`followRaw`) plus `panelBox`/`laneColor` (Task 1) and `dims()` (Task 2).
- Produces: `func WithLogsDir(dir string) Option`, `SnapshotMsg.TranscriptIdents map[string]bool`, `Row.HasTranscript bool`, `ViewMode() == "follow"`.

**Steps**

- [ ] Write the failing tests. Append to `/Users/xlyk/Code/clipse/cli/tui/follow_test.go`:

```go
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

	// While following, the tick keeps the loop alive…
	_, cmd = m.Update(followTickMsg{})
	if cmd == nil {
		t.Error("followTickMsg in follow mode returned nil cmd, want poll+reschedule")
	}
	// …and after esc, it dies.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if got := m.ViewMode(); got != "dashboard" {
		t.Fatalf("after esc ViewMode() = %q, want dashboard", got)
	}
	_, cmd = m.Update(followTickMsg{})
	if cmd != nil {
		t.Error("followTickMsg after leaving follow mode returned a cmd, want nil (tick chain must die)")
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
```

and update `follow_test.go`'s imports to:

```go
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
```

- [ ] Run `go test ./cli/tui/ -race -run 'TestFollow'` — expect a **compile error** (`WithLogsDir`, `m.followPath` undefined).

- [ ] Implement the keys. In `/Users/xlyk/Code/clipse/cli/tui/keys.go`, add two fields to `keyMap` (after `Kanban key.Binding`):

```go
	Follow       key.Binding
	ToggleSource key.Binding
```

add the bindings in `defaultKeyMap()` (after the `Kanban` entry):

```go
		Follow: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "follow logs"),
		),
		ToggleSource: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "transcript/raw"),
		),
```

and surface them in the help — replace `ShortHelp` and `FullHelp`:

```go
// ShortHelp is the single-line hint set (help.KeyMap).
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Follow, k.Kanban, k.Help, k.Quit}
}

// FullHelp is the expanded, column-grouped hint set shown when the help
// overlay is toggled open (help.KeyMap).
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Follow, k.ToggleSource, k.Kanban, k.Help},
		{k.ScrollUp, k.ScrollDown, k.Quit},
	}
}
```

- [ ] Implement the model wiring. In `/Users/xlyk/Code/clipse/cli/tui/model.go`:

Add `"path/filepath"` to the imports.

Add the mode (extend the `viewMode` const block):

```go
const (
	modeDashboard viewMode = iota
	modeDetail
	modeKanban
	modeFollow
)
```

Extend `SnapshotMsg` with one field (after `Live bool`):

```go
	// TranscriptIdents is the set of issue identifiers that already have a
	// transcript file on disk (<logs>/<ISSUE>.transcript.jsonl). It is
	// computed by the refresh command — a directory listing is I/O, so it
	// must not happen inside the pure Update — and follow mode uses it to
	// enable `f` on rows that aren't currently claimed. Nil is fine: no
	// transcripts known.
	TranscriptIdents map[string]bool
```

Add a `Row` field (after `Unmirrored bool` from Task 5):

```go
	// HasTranscript is true when this issue already has a transcript file on
	// disk, so follow mode can replay a past run even when nothing is
	// currently claimed. Sourced from SnapshotMsg.TranscriptIdents.
	HasTranscript bool
```

Add `Model` fields (after the `detailVp viewport.Model` field):

```go
	// Follow mode (U1): logsDir roots both tail paths (WithLogsDir),
	// transcripts is the latest refresh's on-disk transcript set, follow is
	// the active tail session, and followVp scrolls it.
	logsDir     string
	transcripts map[string]bool
	follow      followState
	followVp    viewport.Model
```

Add the option (after `WithRefreshCmd`):

```go
// WithLogsDir points the model at the board's logs directory
// (<board>/logs), the root both follow-mode tail paths derive from:
// <ISSUE>.transcript.jsonl (the structured DAC transcript, written at the
// path dispatcher.transcriptPath builds) and <ISSUE>.log (the worker's raw
// stderr, spawn.LocalSpawner.stderrLogPath). Left unset (tests, misconfig)
// follow mode simply finds no files: the poll reports NotExist and the view
// shows its waiting placeholder.
func WithLogsDir(dir string) Option {
	return func(m *Model) { m.logsDir = dir }
}
```

In `Update`'s `SnapshotMsg` case, record the transcript set before folding:

```go
	case SnapshotMsg:
		m.transcripts = msg.TranscriptIdents
		m.fold(msg.Snap)
		m.live = msg.Live
		m.lastErr = nil
		m.layout()
		return m, nil
```

In `fold`'s `Row` literal, add the field:

```go
			HasTranscript:   m.transcripts[is.Identifier],
```

Add the two follow cases to `Update`'s switch (after the `spinnerTickMsg` case):

```go
	case followTickMsg:
		if m.mode != modeFollow {
			// Follow mode was left since this tick was scheduled: let the
			// chain die rather than polling a file nobody is watching.
			return m, nil
		}
		return m, tea.Batch(pollFollowFile(m.followPath(), m.follow.offset), scheduleFollowTick())

	case followPollMsg:
		if m.mode != modeFollow || msg.Path != m.followPath() {
			return m, nil // stale poll from before a toggle/exit — drop it
		}
		if msg.Err != nil {
			m.follow.err = msg.Err
			return m, nil
		}
		m.follow.err = nil
		if msg.NotExist {
			return m, nil // not written yet — keep polling, placeholder shows
		}
		m.follow.applyChunk(msg.Data, msg.Offset, msg.Reset)
		m.layoutFollow()
		return m, nil
```

Add the key handling. In `handleKey`, add two cases after the `Kanban` case:

```go
	case key.Matches(msg, m.keys.Follow):
		// f opens a realtime tail of the selected issue's agent logs — only
		// for a row a worker is on right now (live claim) or one that
		// already has a transcript on disk (HasTranscript, computed by the
		// refresh cmd; a file check is I/O and can't happen here).
		if m.mode != modeDashboard && m.mode != modeKanban {
			return m, nil
		}
		row, ok := m.selectedRow()
		if !ok || (!row.Live && !row.HasTranscript) {
			return m, nil
		}
		m.mode = modeFollow
		m.follow = followState{ident: row.Identifier, source: followTranscript, pinned: true}
		m.layout()
		return m, tea.Batch(pollFollowFile(m.followPath(), 0), scheduleFollowTick())

	case key.Matches(msg, m.keys.ToggleSource):
		// t flips the tail between the structured transcript and the raw
		// worker stderr. The buffers and offset reset — the two files share
		// nothing — and the next poll (issued immediately) starts from 0.
		if m.mode != modeFollow {
			return m, nil
		}
		src := followRaw
		if m.follow.source == followRaw {
			src = followTranscript
		}
		m.follow = followState{ident: m.follow.ident, source: src, pinned: true}
		m.layout()
		return m, pollFollowFile(m.followPath(), 0)
```

guard `Enter` against follow mode (replace its case body):

```go
	case key.Matches(msg, m.keys.Enter):
		if m.mode != modeDetail && m.mode != modeFollow && m.selected != "" {
			m.mode = modeDetail
			m.detailVp.SetYOffset(0)
			m.layout()
		}
		return m, nil
```

and route Up/Down to the follow viewport (replace both case bodies):

```go
	case key.Matches(msg, m.keys.Up):
		switch m.mode {
		case modeDetail:
			m.detailVp.ScrollUp(1)
		case modeFollow:
			m.followVp.ScrollUp(1)
			m.follow.pinned = m.followVp.AtBottom()
		default:
			m.moveSelection(-1)
			m.layout()
			m.ensureSelectionVisible()
		}
		return m, nil

	case key.Matches(msg, m.keys.Down):
		switch m.mode {
		case modeDetail:
			m.detailVp.ScrollDown(1)
		case modeFollow:
			m.followVp.ScrollDown(1)
			m.follow.pinned = m.followVp.AtBottom()
		default:
			m.moveSelection(1)
			m.layout()
			m.ensureSelectionVisible()
		}
		return m, nil
```

(`esc` needs no change: the existing `Back` case already returns any non-dashboard mode to the dashboard, and the `followTickMsg` guard kills the tick chain.)

Extend `scrollActive` (replace the function):

```go
// scrollActive scrolls whichever viewport the active mode owns by half a
// page in dir (-1 up, +1 down). The viewports clamp to their own content, so
// an over-scroll is a harmless no-op. In follow mode a manual scroll unpins
// the tail unless it lands back on the bottom line.
func (m *Model) scrollActive(dir int) {
	vp := &m.bodyVp
	switch m.mode {
	case modeDetail:
		vp = &m.detailVp
	case modeFollow:
		vp = &m.followVp
	}
	if dir < 0 {
		vp.HalfViewUp()
	} else {
		vp.HalfViewDown()
	}
	if m.mode == modeFollow {
		m.follow.pinned = m.followVp.AtBottom()
	}
}
```

Extend `handleMouse` (add a case before `modeDashboard`):

```go
	case modeFollow:
		m.followVp, _ = m.followVp.Update(msg)
		m.follow.pinned = m.followVp.AtBottom()
```

Extend `ViewMode`:

```go
	case modeFollow:
		return "follow"
```

Add the path/selection helpers (place after `selectedIndex`):

```go
// selectedRow returns the ordered row matching the current selection.
func (m Model) selectedRow() (Row, bool) {
	for _, r := range m.ordered {
		if r.Identifier == m.selected {
			return r, true
		}
	}
	return Row{}, false
}

// followPath is the on-disk file the active follow source tails. Both derive
// from the logs dir runTUI wires in (WithLogsDir): the transcript is
// <logs>/<ISSUE>.transcript.jsonl and the raw stderr log <logs>/<ISSUE>.log,
// both keyed by the issue identifier — matching dispatcher.transcriptPath
// and spawn.LocalSpawner.stderrLogPath exactly.
func (m Model) followPath() string {
	if m.follow.source == followRaw {
		return filepath.Join(m.logsDir, m.follow.ident+".log")
	}
	return filepath.Join(m.logsDir, m.follow.ident+".transcript.jsonl")
}
```

- [ ] Implement the view. In `/Users/xlyk/Code/clipse/cli/tui/view.go`, add the mode to `View`'s switch (before `default`):

```go
	case modeFollow:
		return m.renderFollowScreen(d.cw, now)
```

append `renderFollowScreen` after `renderDashboard`:

```go
// renderFollowScreen draws follow mode (U1): the header, then a full-height
// panel tailing the selected issue's agent logs — the structured transcript
// rendered semantically, or the raw worker stderr — then the pinned footer.
// The panel title carries the issue, the driving lane (colored with the
// lane's identity color once a turn_start has parsed), and the active
// source; a ● marker shows while the tail is pinned to the newest line.
func (m Model) renderFollowScreen(cw int, now int64) string {
	d := m.dims()
	lane := m.follow.lane()
	accent := laneColor(lane)

	src := "transcript"
	if m.follow.source == followRaw {
		src = "raw stderr"
	}
	title := "FOLLOW · " + m.follow.ident
	if lane != "" {
		title += " · " + lane
	}
	title += " · " + src
	if m.follow.pinned {
		title += " · ● streaming"
	}

	panelH := maxInt(d.frameH-d.headerH-d.footerH, 3)
	return lipgloss.JoinVertical(lipgloss.Left,
		m.renderHeader(d.cw, now),
		panelBox(title, accent, m.followVp.View(), d.cw, panelH),
		m.renderFooter(d.cw),
	)
}
```

and add the follow sizing to `layout()` — append at the end of the function (after the detail viewport block), plus the new helper:

```go
	if m.mode == modeFollow {
		m.layoutFollow()
	}
```

```go
// layoutFollow sizes and refills the follow viewport from the tail buffers.
// pinned keeps the view glued to the newest line as content streams in; a
// user who scrolled up stays where they are until they return to the bottom.
func (m *Model) layoutFollow() {
	d := m.dims()
	w := maxInt(d.cw-2, 8)
	m.followVp.Width = w
	m.followVp.Height = maxInt(d.frameH-d.headerH-d.footerH-3, 1)

	var lines []string
	if m.follow.source == followTranscript {
		lines = renderTranscriptLines(m.follow.events, w)
	} else {
		lines = renderRawLines(m.follow.rawLines, w)
	}
	if m.follow.err != nil {
		lines = append(lines, errorStyle.Render(truncatePlain("⚠ "+oneLine(m.follow.err.Error()), w)))
	}
	if len(lines) == 0 {
		lines = []string{dimStyle.Render("waiting for logs…")}
	}
	m.followVp.SetContent(strings.Join(lines, "\n"))
	if m.follow.pinned {
		m.followVp.GotoBottom()
	} else {
		m.followVp.SetYOffset(m.followVp.YOffset) // re-clamp against new bounds
	}
}
```

(`layoutFollow` lives in `view.go` next to `layout`.)

- [ ] Wire `runTUI`. In `/Users/xlyk/Code/clipse/cli/tui.go`, replace the `lockPath := …` / `refresh := …` / `model := …` block (lines 69–80) with:

```go
	lockPath := filepath.Join(boardDir, "clipse.lock")
	logsDir := filepath.Join(boardDir, "logs")
	refresh := func() tea.Msg {
		snap, err := st.ReadSnapshot(context.Background())
		if err != nil {
			return tui.ErrMsg{Err: fmt.Errorf("reading snapshot: %w", err)}
		}
		// Liveness (reading the lockfile) and the transcript listing (one
		// ReadDir) are I/O, so they live here in the refresh command rather
		// than in the pure Update.
		return tui.SnapshotMsg{
			Snap:             snap,
			Live:             dispatcherLive(lockPath),
			TranscriptIdents: transcriptIdents(logsDir),
		}
	}

	model := tui.NewModel(tui.WithRefreshCmd(refresh), tui.WithLogsDir(logsDir))
```

and append the helper after `dispatcherLive`:

```go
// transcriptIdents lists the issue identifiers that already have a
// transcript file on disk (<logs>/<ISSUE>.transcript.jsonl), so the TUI can
// enable follow mode on rows that aren't currently claimed. One ReadDir per
// refresh tick — the same I/O budget class as the snapshot read it rides
// along with. A missing logs dir (no worker has run yet) reads as no
// transcripts.
func transcriptIdents(logsDir string) map[string]bool {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return nil
	}
	idents := make(map[string]bool, len(entries))
	for _, e := range entries {
		if ident, ok := strings.CutSuffix(e.Name(), ".transcript.jsonl"); ok {
			idents[ident] = true
		}
	}
	return idents
}
```

(`cli/tui.go` already imports `os`, `strings`, and `path/filepath`.)

- [ ] Run to green: `go test ./cli/tui/ -race` — all follow tests pass and the full suite stays green. Then `make lint`, then the full gate `make test`.

- [ ] Manual validation (spec acceptance; not automatable here): `clipse tui --board <kept smoke board dir>` during a `--fast` smoke run — `f` on the live row streams the worker, `t` flips to stderr, scroll-up unpins, `esc` returns.

- [ ] Commit:

```
git add cli/tui/model.go cli/tui/keys.go cli/tui/view.go cli/tui/follow_test.go cli/tui.go
git commit -m "feat(tui): follow mode — f tails a live worker, t toggles raw stderr"
```

---

## Self-review

**Spec coverage** — every scope item has a task:
- U1 follow mode → Tasks 7 + 8 (tail via 500ms `tea.Cmd` with byte offset ✓; shrink → offset 0 ✓; transcript semantic rendering for turn_start / tool_call / tool_result / assistant / turn_end / interrupt ✓; malformed lines dim raw, never crash ✓; bottom-pin unless scrolled up ✓; `f` gated on transcript-or-live-claim ✓; `t` toggle ✓; `esc` back ✓; both paths derive from `boardDir` in `runTUI` ✓; zero dispatcher plumbing ✓).
- P1 → Task 2 (content-sized `min(natural, bodyH−actMin)` ✓; empty sections dropped ✓).
- P2 → Task 3 (ACTIVE section, live-first ✓; `⚡ working · ◇ waiting · • queued · ✖ blocked · ✓ done` chips ✓).
- P3 → Task 4 (verdict/mechanic split ✓; stale + retry translations ✓; UUID shortening ✓; lane badges via runs ✓).
- P4 → Task 5 (ReworkCount chip ✓; BlockedUntil countdown ✓; Unmirrored badge + header count ✓; HeartbeatAt warning ✓; Priority sort ✓; RecoverAttempts as `⟳ r<n>` on the rework chip ✓; Description deferred per spec ✓).
- P5 → Task 1 (every constant → AdaptiveColor, dark = current hex ✓).
- P6 → Task 6 (label consistency ✓; kanban active-lane badge + spinner ✓; DONE PR numbers ✓; footer status ✓; `est.` label + two per-lane rates ✓).
- Acceptance unit-test list: pipeline height math ✓ (Task 2), ACTIVE grouping ✓ (Task 3), verdict/mechanic classification + stale translation ✓ (Task 4), tailer offset reset on shrink ✓ (Task 7), transcript-line rendering ✓ (Task 7), adaptive color table ✓ (Task 1), queued priority sort ✓ (Task 5). Manual validation noted in Task 8.

**Issues found and fixed during review:**
- Task 3 initially left `TestFold_ActiveClaimMarksRowLiveWithWorkingLane` building `byID` from `Running()+InFlight()`; fixed to `Active()`.
- Task 4's `kindLabel` originally kept the `claim`-before-`stale` case order, which would have mislabeled `stale_release`; reordered and pinned by test.
- Task 5's retry countdown was first drafted gated on the BLOCKED section per the draft's wording; corrected to any non-live row inside its window, because the kernel sets `blocked_until` on the *re-queued* card (release column), never on the parked `blocked` column — noted as a deliberate deviation.
- Task 7's tool_result "append to the matching call line" was restructured from in-place line mutation to a pure pre-pass (`matchToolResults`) so the renderer stays a pure `events → lines` function and stays unit-testable.
- Task 8's `Enter` handler originally still opened detail from follow mode; guarded.
- Verified no task references a symbol defined only in a later task; Task 6 extends the runs-loop Task 4 introduces and says so explicitly with the full replacement code.

**Placeholder scan**: no TBD/TODO, no "similar to task N", every code-changing step shows complete code. **Symbol consistency**: `Active()`/`waitingCount` (T3) used by T3's header; `laneByRunID` (T4) extended by T6's `laneTokens` loop with full code repeated; `followState`/`pollFollowFile`/`renderTranscriptLines` (T7) consumed by T8 under identical names/signatures; palette names unchanged from v1 so T2–T8 style code compiles against T1's types.

**Known deliberate deviations from the draft (spec wins where they conflict):**
1. `retry_scheduled` translation renders "transient failure — retry n/cap: reason" without the `in <backoff>s` tail — the backoff seconds are not present in the event detail the kernel writes (`dispatcher/reconcile.go` line 207); the live countdown is carried by the row's `BlockedUntil` rendering (P4) instead.
2. The BlockedUntil countdown renders on any non-live row inside its backoff window, not only BLOCKED-section rows (kernel semantics, see Task 5 preamble).
3. `f`-gating uses a `TranscriptIdents` set computed in the refresh command (one `os.ReadDir` per 2s poll) rather than a per-keypress file stat, keeping `Update` pure.
4. The follow panel title carries issue · lane · source · streaming marker; the mockup's per-turn token/elapsed readout lives on the rendered `turn_start`/`turn_end` rules in the body instead of the title.
5. Plan order lands U1 last although the spec ranks it first in value — the spec's constraint list and this plan's task ordering note sanction optimizing for safe incremental landing.

self-review: clean



