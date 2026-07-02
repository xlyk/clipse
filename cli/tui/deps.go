package tui

import "encoding/json"

// parseDeps decodes an Issue.Deps value (a JSON array of dependency issue
// ids, stored as TEXT) into a slice. It is deliberately forgiving: an empty
// string, "[]", or malformed JSON all yield no dependencies rather than an
// error, since a garbled deps column must never break rendering.
func parseDeps(raw string) []string {
	if raw == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return nil
	}
	return ids
}

// depMet reports whether a dependency in board_status status counts as
// satisfied: a dep is met once it reaches a terminal column (done or
// cancelled), which is exactly when board.Promote stops treating it as
// blocking. A dep whose issue is unknown (absent from the snapshot) is treated
// as unmet — better to show a card as waiting than to falsely mark it ready.
func depMet(status string) bool {
	return status == "done" || status == "cancelled"
}

// unmetDeps resolves an issue's raw deps to the identifiers of the
// dependencies that are not yet terminal, using the snapshot-derived
// id→identifier and id→status maps. It returns the identifiers in dependency
// order; a dep whose identifier is unknown falls back to a short id so the row
// still says something useful.
func unmetDeps(rawDeps string, identByID, statusByID map[string]string) []string {
	var unmet []string
	for _, id := range parseDeps(rawDeps) {
		if depMet(statusByID[id]) {
			continue
		}
		if ident := identByID[id]; ident != "" {
			unmet = append(unmet, ident)
		} else {
			unmet = append(unmet, shortID(id))
		}
	}
	return unmet
}

// blockerState is one dependency resolved for the detail view: its display
// identifier and whether it is satisfied.
type blockerState struct {
	Identifier string
	Met        bool
}

// blockers resolves an issue's raw deps into ordered blockerState values for
// the detail view's "blocked-by" line, preserving every dep (met or not) so
// the reader sees the whole dependency set with a per-dep checkmark.
func blockers(rawDeps string, identByID, statusByID map[string]string) []blockerState {
	ids := parseDeps(rawDeps)
	states := make([]blockerState, 0, len(ids))
	for _, id := range ids {
		ident := identByID[id]
		if ident == "" {
			ident = shortID(id)
		}
		states = append(states, blockerState{Identifier: ident, Met: depMet(statusByID[id])})
	}
	return states
}

// shortID truncates a UUID-ish id to its first segment for display when no
// human identifier is known for it.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Rough, display-only blended token prices (Sonnet-class $/token). These exist
// solely to turn cumulative token counts into a ballpark "$ spent" figure in
// the header; they are NOT billing-accurate and are intentionally coarse.
const (
	costPerInputToken  = 3.0 / 1_000_000  // ~$3 per 1M input tokens
	costPerOutputToken = 15.0 / 1_000_000 // ~$15 per 1M output tokens
)

// estimateCostUSD returns a rough dollar estimate for the given cumulative
// token totals using the blended display rates above.
func estimateCostUSD(tokensIn, tokensOut int) float64 {
	return float64(tokensIn)*costPerInputToken + float64(tokensOut)*costPerOutputToken
}
