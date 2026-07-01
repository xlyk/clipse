package linear

import "strings"

// laneLabelPrefix is the Linear label prefix that marks a lane label, e.g.
// "agent:coder". This mirrors config.defaultLaneLabelPrefix; it is
// duplicated here (rather than imported) to keep this package dependency-free
// of internal/config. If the prefix ever becomes configurable per-repo, this
// value should move to a Normalize parameter instead of a constant.
const laneLabelPrefix = "agent:"

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
	"todo":          "todo",
	"backlog":       "todo",
	"ready":         "ready",
	"running":       "running",
	"in progress":   "running",
	"review":        "review",
	"in review":     "review",
	"merging":       "merging",
	"documentation": "documentation",
	"done":          "done",
	"rework":        "rework",
	"blocked":       "blocked",
}

// statusFromWorkflowName maps a Linear workflow-state name to our Column
// enum value, matching case-insensitively. Unrecognized names map to "todo".
func statusFromWorkflowName(name string) string {
	if col, ok := statusByWorkflowName[strings.ToLower(name)]; ok {
		return col
	}
	return "todo"
}

// laneFromLabels scans Linear label names for an "agent:<lane>" label and
// returns the bare lane with the prefix stripped. Returns "" if no such
// label is present; callers must treat that as "no lane assigned" rather
// than an error.
func laneFromLabels(labelNames []string) string {
	for _, name := range labelNames {
		if rest, ok := strings.CutPrefix(name, laneLabelPrefix); ok && rest != "" {
			return rest
		}
	}
	return ""
}
