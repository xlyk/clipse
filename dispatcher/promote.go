package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xlyk/clipse/internal/board"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/store"
)

// terminalStatuses are the board columns board.Promote treats as "this
// dependency will never re-enter an active column" (see board.DepState.
// Terminal).
var terminalStatuses = map[string]bool{
	string(contract.ColumnDone): true,
	"cancelled":                 true,
}

// promote reads one snapshot of the store and moves every Todo issue whose
// dependencies are all terminal to Ready, mirroring the promotion to Linear
// via the outbox. It reads the snapshot once up front (rather than
// per-issue) so promotion decisions within a single tick are made against a
// consistent view, even though the transitions themselves are applied one at
// a time.
func (d *Dispatcher) promote(ctx context.Context) error {
	snap, err := d.store.ReadSnapshot(ctx)
	if err != nil {
		return fmt.Errorf("reading snapshot: %w", err)
	}

	terminalByID := make(map[string]bool, len(snap.Issues))
	for _, is := range snap.Issues {
		terminalByID[is.ID] = terminalStatuses[is.BoardStatus]
	}

	now := d.now()
	for _, is := range snap.Issues {
		if is.BoardStatus != string(contract.ColumnTodo) {
			continue
		}

		var depIDs []string
		if err := json.Unmarshal([]byte(is.Deps), &depIDs); err != nil {
			return fmt.Errorf("decoding deps for issue %s: %w", is.ID, err)
		}
		depStates := make([]board.DepState, len(depIDs))
		for i, depID := range depIDs {
			depStates[i] = board.DepState{Terminal: terminalByID[depID]}
		}

		if !board.Promote(is.BoardStatus, depStates) {
			continue
		}

		req := store.TransitionReq{
			IssueID:         is.ID,
			NewStatus:       string(contract.ColumnReady),
			EnqueueSetState: true,
			Event: store.Event{
				Ts:      now,
				IssueID: nullString(is.ID),
				Kind:    "promoted",
				Detail:  fmt.Sprintf("issue %s promoted todo -> ready (deps terminal)", is.ID),
			},
		}
		if err := d.store.Transition(ctx, req); err != nil {
			return fmt.Errorf("promoting issue %s: %w", is.ID, err)
		}
	}
	return nil
}
