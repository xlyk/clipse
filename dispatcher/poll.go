package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xlyk/clipse/internal/store"
)

// pollAndUpsert fetches candidate issues from Linear and caches them in the
// store via UpsertIssue. UpsertIssue's own conflict semantics (preserve
// board_status/claim on conflict) are what make a re-poll safe against an
// in-flight claim — this method just needs to map fields, not worry about
// clobbering dispatcher-owned state.
//
// Issues with no lane assigned (Lane == "") are cached (so their identifier/
// deps/priority stay current for when a lane label is eventually added) but
// are otherwise inert: selectAndClaim only ever claims from a specific lane
// label, so an empty lane_label issue is never a candidate to claim.
func (d *Dispatcher) pollAndUpsert(ctx context.Context) error {
	issues, err := d.linear.CandidateIssues(ctx)
	if err != nil {
		return fmt.Errorf("polling candidate issues: %w", err)
	}

	now := d.now()
	for _, li := range issues {
		deps, err := json.Marshal(li.Deps)
		if err != nil {
			return fmt.Errorf("encoding deps for issue %s: %w", li.Identifier, err)
		}

		row := store.Issue{
			ID:          li.ID,
			Identifier:  li.Identifier,
			LaneLabel:   li.Lane,
			BoardStatus: li.Status,
			Deps:        string(deps),
			Priority:    li.Priority,
			BranchName:  li.BranchName,
			UpdatedAt:   li.UpdatedAt,
			LastSeen:    now,
			CreatedAt:   now,
		}
		if err := d.store.UpsertIssue(ctx, row); err != nil {
			return fmt.Errorf("caching issue %s: %w", li.Identifier, err)
		}
	}
	return nil
}
