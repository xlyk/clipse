// Package dispatcher implements the dispatch daemon and its reconciliation loop.
package dispatcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// Workspacer is the seam the dispatcher uses to get a worktree for a claimed
// issue and to clean it up once the issue reaches a terminal state. The
// production implementation (gitWorkspacer) wraps spawn.EnsureWorktree /
// spawn.RemoveWorktree; tests substitute a stub that returns a t.TempDir.
type Workspacer interface {
	// Ensure returns the workspace directory for issue, creating it if
	// necessary.
	Ensure(issue store.Issue) (string, error)

	// Remove tears down the workspace for issue (called on terminal states).
	Remove(issue store.Issue) error
}

// inflightRun tracks one runID the dispatcher has spawned but not yet
// reconciled. It is read/written only on the Tick goroutine — see the
// concurrency-model note on Dispatcher.
type inflightRun struct {
	handle    spawn.RunHandle
	issueID   string
	lane      string
	workspace string
	cancel    context.CancelFunc
	turn      int
}

// runResult is what the Wait-goroutine sends back to the Tick goroutine once
// a spawned worker exits (cleanly, by crashing, or by context deadline).
type runResult struct {
	runID   string
	issueID string
	res     spawn.Result
}

// Dispatcher runs one deterministic scheduling pass (Tick) at a time: it
// polls Linear, reconciles finished runs, promotes ready-eligible issues,
// claims and spawns new work up to configured caps, and drains the outbox of
// pending Linear mirror writes.
//
// Concurrency model: spawning a worker starts exactly one goroutine, which
// blocks on handle.Wait() and then sends a runResult on d.results. Tick never
// blocks on Wait — it drains d.results with a non-blocking loop instead. The
// inflight map is touched only by the Tick goroutine, so beyond the
// channel, no mutable state is shared across goroutines.
type Dispatcher struct {
	cfg     config.Config
	store   *store.Store
	linear  linear.Client
	spawner spawn.Spawner
	ws      Workspacer

	now      func() int64
	newRunID func() string
	envFor   func(issue store.Issue) []string

	logger *slog.Logger

	results  chan runResult
	inflight map[string]inflightRun
}

// Option configures optional Dispatcher fields at construction. Most callers
// only need New's required deps; tests use these to substitute a fixed
// clock, a deterministic runID generator, or a custom worker environment.
type Option func(*Dispatcher)

// WithClock overrides the default time.Now-based clock.
func WithClock(now func() int64) Option {
	return func(d *Dispatcher) { d.now = now }
}

// WithRunIDGenerator overrides the default crypto/rand-based run id
// generator.
func WithRunIDGenerator(gen func() string) Option {
	return func(d *Dispatcher) { d.newRunID = gen }
}

// WithEnvFor sets the function used to build a spawned worker's environment
// for a given issue. Defaults to nil (the worker inherits no extra env
// beyond whatever the Spawner implementation itself sets).
func WithEnvFor(envFor func(issue store.Issue) []string) Option {
	return func(d *Dispatcher) { d.envFor = envFor }
}

// WithLogger overrides the default slog logger used for dispatcher lifecycle
// logging.
func WithLogger(logger *slog.Logger) Option {
	return func(d *Dispatcher) { d.logger = logger }
}

// WithResultsBuffer overrides the default results channel buffer size.
func WithResultsBuffer(n int) Option {
	return func(d *Dispatcher) { d.results = make(chan runResult, n) }
}

// defaultResultsBuffer sizes the results channel comfortably above
// cfg.Caps.Global in the common case; Tick's non-blocking drain never blocks
// on a full channel anyway (the Wait-goroutine send would just block a
// little longer), but a generous buffer keeps that from ever mattering.
const defaultResultsBuffer = 256

// New constructs a Dispatcher from its required dependencies, applying any
// Options on top of the defaults (a real time.Now clock, a crypto/rand hex
// run id generator, no extra worker env, and slog.Default()).
func New(cfg config.Config, st *store.Store, lc linear.Client, spawner spawn.Spawner, ws Workspacer, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		cfg:      cfg,
		store:    st,
		linear:   lc,
		spawner:  spawner,
		ws:       ws,
		now:      func() int64 { return time.Now().Unix() },
		newRunID: newRandomRunID,
		logger:   slog.Default(),
		results:  make(chan runResult, defaultResultsBuffer),
		inflight: make(map[string]inflightRun),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// newRandomRunID generates a unique run id via crypto/rand: 16 random bytes
// hex-encoded, so concurrent claims across lanes never collide.
func newRandomRunID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand.Read failing is effectively unrecoverable (no entropy
		// source); panic rather than silently degrade run-id uniqueness.
		panic(fmt.Errorf("generating run id: %w", err))
	}
	return hex.EncodeToString(buf)
}

// ttl returns the claim TTL (seconds) used for ClaimReady/Heartbeat: the
// configured max runtime, so a claim stays alive for exactly as long as the
// worker is allowed to run.
func (d *Dispatcher) ttl() int64 {
	return int64(d.cfg.MaxRuntimeS)
}

// Tick runs one scheduling pass: poll, reconcile, promote, claim+spawn, drain
// outbox, in that fixed order. Each phase is a small helper method so the
// overall pass reads as a checklist; see the package doc and the phase
// methods themselves for the reasoning behind the ordering.
func (d *Dispatcher) Tick(ctx context.Context) error {
	if err := d.pollAndUpsert(ctx); err != nil {
		return fmt.Errorf("tick: poll: %w", err)
	}
	if err := d.reconcile(ctx); err != nil {
		return fmt.Errorf("tick: reconcile: %w", err)
	}
	if err := d.promote(ctx); err != nil {
		return fmt.Errorf("tick: promote: %w", err)
	}
	if err := d.selectAndClaim(ctx); err != nil {
		return fmt.Errorf("tick: select and claim: %w", err)
	}
	if err := d.drainOutbox(ctx); err != nil {
		return fmt.Errorf("tick: drain outbox: %w", err)
	}
	return nil
}

// laneCaps returns the ordered set of lanes the dispatcher schedules, paired
// with each lane's configured per-lane cap. The bare lane values (e.g.
// "coder") are what issues.lane_label / runs.lane store, per the store
// package's documented convention — NOT the "agent:coder" Linear label.
func (d *Dispatcher) laneCaps() []laneCap {
	return []laneCap{
		{lane: string(contract.LaneCoder), cap: d.cfg.Caps.PerLane.Coder},
		{lane: string(contract.LaneReviewer), cap: d.cfg.Caps.PerLane.Reviewer},
		{lane: string(contract.LaneGitOperator), cap: d.cfg.Caps.PerLane.GitOperator},
		{lane: string(contract.LaneScribe), cap: d.cfg.Caps.PerLane.Scribe},
	}
}

type laneCap struct {
	lane string
	cap  int
}

// inflightLaneCounts returns the current in-flight run count, both globally
// and per lane, computed from d.inflight (Tick-goroutine-only state, so this
// is safe to call without extra synchronization from within Tick).
func (d *Dispatcher) inflightLaneCounts() (global int, perLane map[string]int) {
	perLane = make(map[string]int)
	for _, r := range d.inflight {
		global++
		perLane[r.lane]++
	}
	return global, perLane
}
