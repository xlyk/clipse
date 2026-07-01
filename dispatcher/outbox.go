package dispatcher

import (
	"context"
	"fmt"
)

// drainOutboxLimit bounds how many pending linear_writes rows drainOutbox
// processes per tick, so one tick can't be dominated by an enormous backlog.
const drainOutboxLimit = 100

// drainOutbox is the only place the dispatcher writes to Linear: every board
// transition enqueues a pending mirror write in the same store transaction
// that changed board state, and drainOutbox is what actually sends it. A
// Linear API failure here just leaves the row pending for a later tick's
// retry — it never loses or duplicates the underlying transition (A2).
func (d *Dispatcher) drainOutbox(ctx context.Context) error {
	writes, err := d.store.DrainPendingLinearWrites(ctx, drainOutboxLimit)
	if err != nil {
		return fmt.Errorf("draining pending linear writes: %w", err)
	}

	for _, w := range writes {
		var sendErr error
		switch w.Kind {
		case "setstate":
			sendErr = d.linear.SetState(ctx, w.IssueID, w.Target)
		case "comment":
			sendErr = d.linear.Comment(ctx, w.IssueID, w.Body)
		default:
			sendErr = fmt.Errorf("unknown linear write kind %q", w.Kind)
		}

		if sendErr != nil {
			if err := d.store.MarkLinearWriteFailed(ctx, w.ID, sendErr.Error()); err != nil {
				return fmt.Errorf("marking linear write %d failed: %w", w.ID, err)
			}
			d.logger.Warn("linear mirror write failed, will retry", "write_id", w.ID, "issue_id", w.IssueID, "kind", w.Kind, "error", sendErr)
			continue
		}
		if err := d.store.MarkLinearWriteDone(ctx, w.ID); err != nil {
			return fmt.Errorf("marking linear write %d done: %w", w.ID, err)
		}
	}
	return nil
}
