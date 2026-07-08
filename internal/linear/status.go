package linear

import "strings"

// statusByWorkflowName is the single documented mapping from a Linear
// workflow-state NAME (case-insensitive) to our board Column enum
// (internal/contract.Column). Keep this map in sync with
// schema/board.schema.json's Column enum and with the workflow states
// configured on the Linear team driving Clipse.
//
// Names not present here fall back to "todo" (see statusFromWorkflowName)
// so an unrecognized/renamed Linear state doesn't crash normalization; it
// just won't be picked up as ready/running/etc until the mapping is fixed.
var statusByWorkflowName = map[string]string{
	"todo":        "todo",
	"backlog":     "todo",
	"ready":       "ready",
	"running":     "running",
	"in progress": "running",
	"review":      "review",
	"in review":   "review",
	"merging":     "merging",
	"done":        "done",
	"rework":      "rework",
	"blocked":     "blocked",
}

// statusFromWorkflowName maps a Linear workflow-state name (and its fixed
// state TYPE) to our board status. stateType is checked first and takes
// priority: Linear's six state types (backlog/unstarted/started/completed/
// canceled/triage) are a closed, unrenameable vocabulary, unlike a state's
// display NAME, which a team can call anything ("Won't Fix", "Abandoned",
// ...). Name-matching alone (as every other column here still does) would be
// fragile specifically for cancellation: an unrecognized name falls back to
// "todo" (see below), which would make a cancelled blocker look ACTIVE
// instead of terminal -- the opposite of what dispatcher/promote.go's
// dependency gating needs. "cancelled" (double-l) is deliberately not a
// contract.Column value; issues.board_status is unconstrained TEXT (see
// internal/store/migrations.go), and dispatcher/promote.go +
// dispatcher/recover.go already special-case this exact string as terminal.
//
// Names not present in statusByWorkflowName fall back to "todo" so an
// unrecognized/renamed Linear state doesn't crash normalization; it just
// won't be picked up as ready/running/etc until the mapping is fixed.
func statusFromWorkflowName(name, stateType string) string {
	if stateType == "canceled" {
		return "cancelled"
	}
	if stateType == "completed" {
		// Same type-over-name rationale as canceled: a completed-type state
		// with a team-specific name ("Ready for Release", "Closed") must
		// never fall back to todo -- that would let a mislabeled,
		// already-shipped ticket be claimed and re-run. Name-mapped
		// completed states ("Done") resolve to "done" either way.
		return "done"
	}
	if col, ok := statusByWorkflowName[strings.ToLower(name)]; ok {
		return col
	}
	return "todo"
}

// canonicalWorkflowNameByColumn is the (single-valued) inverse of
// statusByWorkflowName, keyed by the bare board Column string (e.g.
// "review"): it names the one canonical Linear workflow-state NAME
// HTTPClient.SetState resolves that column to when moving a card (see
// state_resolver.go). Several Linear state names can fold onto the same
// Column (e.g. "todo" and "backlog" both mean "todo"), so inverting
// statusByWorkflowName mechanically would be ambiguous; this map picks one
// canonical name per column instead. Keep in sync with statusByWorkflowName
// and the workflow states actually configured on the Linear team driving
// Clipse.
var canonicalWorkflowNameByColumn = map[string]string{
	"todo":    "Todo",
	"ready":   "Ready",
	"running": "Running",
	"review":  "Review",
	"merging": "Merging",
	"done":    "Done",
	"rework":  "Rework",
	"blocked": "Blocked",
}

// canonicalWorkflowName returns the canonical Linear workflow-state name for
// column (a bare board Column string, e.g. "review"), and whether column
// was recognized as one of our board columns.
func canonicalWorkflowName(column string) (string, bool) {
	name, ok := canonicalWorkflowNameByColumn[column]
	return name, ok
}

// laneFromLabels scans Linear label names for a "<labelPrefix><lane>" label
// (e.g. "agent:coder") and returns the bare lane with the prefix stripped.
// labelPrefix comes from config.Config.LaneLabelPrefix, threaded through from
// HTTPClient's construction (cli/dispatch.go) -- this package stays
// dependency-free of internal/config (no import), so it takes the resolved
// string rather than re-deriving config's own default. Returns "" if no such
// label is present; callers must treat that as "no lane assigned" rather
// than an error.
func laneFromLabels(labelNames []string, labelPrefix string) string {
	for _, name := range labelNames {
		if rest, ok := strings.CutPrefix(name, labelPrefix); ok && rest != "" {
			return rest
		}
	}
	return ""
}
