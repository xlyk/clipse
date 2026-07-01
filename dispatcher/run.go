package dispatcher

import (
	"context"
	"fmt"
	"time"
)

// WithPollInterval overrides the default Tick interval Run uses (which is
// otherwise time.Duration(cfg.PollIntervalS)*time.Second). Tests use this to
// drive Run with a tiny interval instead of waiting on a real poll cadence.
func WithPollInterval(d time.Duration) Option {
	return func(dd *Dispatcher) { dd.pollInterval = d }
}

// pollInterval returns the configured Tick interval: d.pollInterval if an
// Option set it, otherwise cfg.PollIntervalS as a time.Duration.
func (d *Dispatcher) pollIntervalOrDefault() time.Duration {
	if d.pollInterval > 0 {
		return d.pollInterval
	}
	return time.Duration(d.cfg.PollIntervalS) * time.Second
}

// Run is the dispatcher's daemon loop: it recovers any orphaned runs left by
// a prior dispatcher process exactly once, then runs Tick immediately and
// again on every pollInterval tick, until ctx is cancelled.
//
// A Tick error is logged and looped past rather than returned: a single
// transient failure (e.g. a momentary Linear API blip) should not bring the
// whole daemon down when the next tick is likely to succeed. Run itself only
// returns a non-nil error if RecoverOrphans fails at startup, since that is a
// one-time precondition the rest of the loop depends on.
//
// On ctx.Done(), Run stops the ticker and returns nil. It does not cancel
// any in-flight worker spawn contexts — see dispatcher/spawn.go's use of
// context.WithoutCancel — so a graceful shutdown (e.g. SIGINT/SIGTERM) lets
// live workers run to completion. If the process exits while a worker is
// still live, that worker becomes an orphan the next dispatcher's
// RecoverOrphans reaps.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.logger.Info("dispatcher starting", "poll_interval", d.pollIntervalOrDefault())

	if err := d.RecoverOrphans(ctx); err != nil {
		return fmt.Errorf("run: recovering orphans at startup: %w", err)
	}

	ticker := time.NewTicker(d.pollIntervalOrDefault())
	defer ticker.Stop()

	d.tickAndLog(ctx)

	for {
		select {
		case <-ctx.Done():
			d.logger.Info("dispatcher shutting down gracefully")
			return nil
		case <-ticker.C:
			d.tickAndLog(ctx)
		}
	}
}

// tickAndLog runs one Tick, logging (but not propagating) any error: see
// Run's doc comment for why a transient Tick failure keeps the daemon
// looping instead of exiting.
func (d *Dispatcher) tickAndLog(ctx context.Context) {
	if err := d.Tick(ctx); err != nil {
		d.logger.Error("tick failed", "error", err)
	}
}
