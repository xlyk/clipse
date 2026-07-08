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
