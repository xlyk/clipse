package tui

import (
	"sort"

	"github.com/xlyk/clipse/internal/store"
)

// sortedIssueSnapshots returns issues ordered by Identifier, mirroring
// cli.RenderStatus's approach: Go's map/slice iteration order isn't
// guaranteed stable across ReadSnapshot calls, so fold sorts before
// grouping to keep each section's row order deterministic.
func sortedIssueSnapshots(issues []store.IssueSnapshot) []store.IssueSnapshot {
	sorted := make([]store.IssueSnapshot, len(issues))
	copy(sorted, issues)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Identifier < sorted[j].Identifier
	})
	return sorted
}
