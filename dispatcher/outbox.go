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
// The store returns only each issue's oldest pending write. A failure leaves
// that head pending while independent issues continue; later writes for the
// failed issue cannot overtake it or monopolize this batch.
func (d *Dispatcher) drainOutbox(ctx context.Context) error {
	now := d.now()
	blockedIssues := make(map[string]bool)
	processed := 0
	for processed < drainOutboxLimit {
		writes, err := d.store.DrainPendingLinearWriteHeads(ctx, drainOutboxLimit)
		if err != nil {
			return fmt.Errorf("draining pending linear writes: %w", err)
		}
		if len(writes) == 0 {
			return nil
		}

		madeProgress := false
		for _, w := range writes {
			if processed >= drainOutboxLimit {
				break
			}
			if blockedIssues[w.IssueID] {
				continue
			}
			madeProgress = true
			processed++

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
				if err := d.store.MarkLinearWriteFailed(ctx, w.ID, sendErr.Error(), now); err != nil {
					return fmt.Errorf("marking linear write %d failed: %w", w.ID, err)
				}
				blockedIssues[w.IssueID] = true
				d.logger.Warn("linear mirror write failed, will retry", "write_id", w.ID, "issue_id", w.IssueID, "kind", w.Kind, "error", sendErr)
				continue
			}
			if err := d.store.MarkLinearWriteDone(ctx, w.ID, now); err != nil {
				return fmt.Errorf("marking linear write %d done: %w", w.ID, err)
			}
		}
		if !madeProgress {
			return nil
		}
	}
	return nil
}
