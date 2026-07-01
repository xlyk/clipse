package dispatcher

import (
	"context"
	"errors"
	"fmt"

	"github.com/xlyk/clipse/internal/store"
)

// selectAndClaim claims and spawns as much ready work as the configured caps
// allow, one lane at a time. Both the global cap and each lane's per-lane cap
// are re-checked before every single claim, so a claim made mid-loop for one
// lane immediately counts against the global cap for every other lane
// checked afterward in the same pass.
func (d *Dispatcher) selectAndClaim(ctx context.Context) error {
	now := d.now()
	for _, lc := range d.laneCaps() {
		for {
			global, perLane := d.inflightLaneCounts()
			if global >= d.cfg.Caps.Global {
				break
			}
			if perLane[lc.lane] >= lc.cap {
				break
			}

			// issues.lane_label / runs.lane store the BARE lane (e.g.
			// "coder"), not the "agent:coder" Linear label — LaneLabelPrefix
			// is only relevant when parsing Linear labels (internal/linear).
			claim, err := d.store.ClaimReady(ctx, lc.lane, d.newRunID(), now, d.ttl())
			if errors.Is(err, store.ErrNoReady) {
				break
			}
			if err != nil {
				return fmt.Errorf("claiming ready issue in lane %s: %w", lc.lane, err)
			}

			// Mirror the claim's ready->running move to Linear. ClaimReady's
			// CAS is not itself a Transition call, so nothing else enqueues
			// this outbox row.
			if err := d.store.EnqueueLinearSetState(ctx, claim.Issue.ID, "running", now); err != nil {
				return fmt.Errorf("enqueueing running mirror for issue %s: %w", claim.Issue.ID, err)
			}

			if err := d.spawnClaim(ctx, *claim); err != nil {
				return fmt.Errorf("spawning claim for issue %s: %w", claim.Issue.ID, err)
			}
		}
	}
	return nil
}

// spawnClaim starts the worker process for a freshly won claim. A Spawn
// failure (e.g. workspace setup or exec failure, as opposed to a worker
// process failure) is treated the same as an in-run failure: the issue is
// blocked immediately, since there is no process to wait on.
func (d *Dispatcher) spawnClaim(ctx context.Context, claim store.Claim) error {
	return d.spawnAttempt(ctx, claim.Issue, claim.Run.RunID, claim.Run.Lane, "", 1)
}
