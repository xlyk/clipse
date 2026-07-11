// Package dispatcher implements the dispatch daemon and its reconciliation loop.
package dispatcher

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/config"
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
	// necessary. Used by the Coder/Reviewer lanes and by the inline
	// Git-operator: all three operate on the issue's own (coder) branch.
	// (Documentation is written inside the Coder lane's own turn, in this
	// same worktree, so there is no separate docs-branch workspace.)
	Ensure(issue store.Issue) (string, error)

	// Remove idempotently removes issue's worktree and local branch. Terminal
	// local cleanup may be retried after a partial failure or restart.
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
	backend backend.Manager

	now      func() int64
	newRunID func() string
	envFor   func(issue store.Issue) []string

	// gitOps runs the deterministic Git-operator lane inline for a claimed
	// "merging" card (design decision J amendment / O: the lane's executor
	// is kernel code, never a spawned DAC worker). Defaults to
	// defaultGitOpsRunner (a real gitops.Run call); tests substitute a fake
	// via WithGitOpsRunner so the merging path never touches a real gh/git
	// subprocess.
	gitOps GitOpsRunner

	// gitOpsPreCheck runs the READ-ONLY PR pre-check runGitopsClaim performs
	// from the primary clone before ensuring a worktree (a merged/missing PR
	// short-circuits without resurrecting a hand-deleted worktree). Defaults to
	// defaultGitOpsPreChecker (a real gitops.PreCheckPRState call); tests
	// substitute a fake via WithGitOpsPreChecker.
	gitOpsPreCheck GitOpsPreChecker

	logger *slog.Logger

	// pollInterval overrides the Tick interval Run uses when set (via
	// WithPollInterval); zero means "use cfg.PollIntervalS" — see
	// pollIntervalOrDefault in run.go.
	pollInterval time.Duration

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
// for a given issue, overriding New's default (see defaultEnvFor). Tests use
// this to substitute a fixed or scenario-specific environment (e.g. to drive
// testworker's TESTWORKER_SCENARIO directly) without going through
// cfg.EnvAllowlist.
func WithEnvFor(envFor func(issue store.Issue) []string) Option {
	return func(d *Dispatcher) { d.envFor = envFor }
}

// WithBackendManager installs the provider-neutral remote workspace manager.
// Local mode never consults it; the dispatch composition root supplies it
// only when agent_backend.type is daytona.
func WithBackendManager(manager backend.Manager) Option {
	return func(d *Dispatcher) { d.backend = manager }
}

// WithLogger overrides the default slog logger used for dispatcher lifecycle
// logging.
func WithLogger(logger *slog.Logger) Option {
	return func(d *Dispatcher) { d.logger = logger }
}

// WithGitOpsRunner overrides the default Git-operator lane executor (see
// Dispatcher.gitOps). Tests use this to substitute a fake that returns
// scripted gitops.Result values instead of running gh/git for real.
func WithGitOpsRunner(fn GitOpsRunner) Option {
	return func(d *Dispatcher) { d.gitOps = fn }
}

// WithGitOpsPreChecker overrides the default read-only PR pre-checker (see
// Dispatcher.gitOpsPreCheck). Tests use this to script whether the pre-check
// resolves the pass (merged/missing PR) or falls through to the worktree run,
// without touching a real gh subprocess.
func WithGitOpsPreChecker(fn GitOpsPreChecker) Option {
	return func(d *Dispatcher) { d.gitOpsPreCheck = fn }
}

// WithResultsBuffer overrides the default results channel buffer size.
func WithResultsBuffer(n int) Option {
	return func(d *Dispatcher) { d.results = make(chan runResult, n) }
}

// defaultResultsBuffer is the floor New sizes d.results to (see
// resultsBufferSize): at least this many slots regardless of cfg.Caps.Global,
// generous for the common case.
//
// A full buffer is not harmless. Tick's own drain (drainResults) never blocks
// on it -- but applyResult's GetIssue-failure path (dispatcher/reconcile.go)
// requeues a result it can't yet apply by sending it back onto this exact
// channel, from the Tick goroutine, which is also the channel's ONLY reader.
// A blocking send there with a full buffer deadlocks the dispatcher forever:
// the one goroutine that could ever drain the channel is stuck sending to it.
// resultsBufferSize's cfg.Caps.Global+1 floor is the first line of defense
// (room for one in-flight result per concurrently-running worker plus one
// spare requeue slot); applyResult's own non-blocking send with a
// leave-it-inflight fallback is the second, for the case this sizing
// invariant is somehow violated anyway.
const defaultResultsBuffer = 256

// resultsBufferSize returns the capacity New gives d.results: at least
// defaultResultsBuffer, but never less than cfg.Caps.Global+1 so the channel
// can hold one result per concurrently-running worker with a spare slot free
// for applyResult's same-tick requeue — see defaultResultsBuffer's doc
// comment for why that spare slot matters.
func resultsBufferSize(cfg config.Config) int {
	return max(defaultResultsBuffer, cfg.Caps.Global+1)
}

// New constructs a Dispatcher from its required dependencies, applying any
// Options on top of the defaults (a real time.Now clock, a crypto/rand hex
// run id generator, an allow-listed worker env per defaultEnvFor, and
// slog.Default()).
func New(cfg config.Config, st *store.Store, lc linear.Client, spawner spawn.Spawner, ws Workspacer, opts ...Option) *Dispatcher {
	d := &Dispatcher{
		cfg:            cfg,
		store:          st,
		linear:         lc,
		spawner:        spawner,
		ws:             ws,
		now:            func() int64 { return time.Now().Unix() },
		newRunID:       newRandomRunID,
		envFor:         defaultEnvFor(cfg.EnvAllowlist),
		gitOps:         defaultGitOpsRunner,
		gitOpsPreCheck: defaultGitOpsPreChecker,
		logger:         slog.Default(),
		results:        make(chan runResult, resultsBufferSize(cfg)),
		inflight:       make(map[string]inflightRun),
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// defaultEnvFor is New's default envFor, overridable via WithEnvFor: it
// returns the dispatcher's own environment filtered down to allowlist via
// spawn.AllowlistedEnv (config.Config.EnvAllowlist — Phase 2 has no
// per-lane secret differences yet), plus one dispatcher-computed addition:
// CLIPSE_ISSUE_TEXT, set from issue's own title/description (see issueText).
// The filtered env is what keeps a spawned worker from ever inheriting
// LINEAR_API_KEY or anything else the dispatcher process holds that isn't
// explicitly allow-listed (design doc threat model, B3) — closing the gap
// where an unset envFor previously left WorkerSpec.Env nil, which
// exec.Cmd.Env treats as "inherit everything". CLIPSE_ISSUE_TEXT closes a
// second gap: it's not copied from the dispatcher's own environment at all
// (an allowlist entry could never produce it), so it's appended unconditionally
// rather than run through AllowlistedEnv — otherwise the Coder lane's
// graphs/coder.py load_context has no task text to give the DAC agent and
// every run fails with "user messages must have non-empty content".
func defaultEnvFor(allowlist []string) func(store.Issue) []string {
	return func(issue store.Issue) []string {
		env := spawn.AllowlistedEnv(os.Environ(), allowlist)
		return append(env, clipseIssueTextEnvVar+"="+issueText(issue))
	}
}

// clipseIssueTextEnvVar is the environment variable the Coder lane's
// graphs/coder.py load_context falls back to reading
// (os.environ.get("CLIPSE_ISSUE_TEXT", "")) when the worker invocation
// didn't pass issue_text directly — which worker.py never does, so this env
// var is the ONLY production path that gets an issue's task text to the
// worker.
const clipseIssueTextEnvVar = "CLIPSE_ISSUE_TEXT"

// clipseReviewFeedbackEnvVar carries the most recent review/rework feedback
// into a Coder lane re-run claimed out of the rework column: the summary of
// the run that routed the card there (a Reviewer's changes_requested, or a
// Git-operator stale-base conflict -- see store.LatestReworkFeedback).
// graphs/coder.py load_context folds it into the DAC prompt so the Coder
// actually acts on the feedback instead of re-emitting a byte-identical diff.
// Like CLIPSE_ISSUE_TEXT it is injected directly by the dispatcher at spawn
// time (spawnAttempt), never sourced from the host environment, so it bypasses
// the EnvAllowlist scrubbing entirely; a fresh coder claim from ready never
// sets it.
const clipseReviewFeedbackEnvVar = "CLIPSE_REVIEW_FEEDBACK"

// clipseDependencyNotesEnvVar carries a coder claim's dependency-comment
// history into the worker: its blockers' Linear comments (the decisions the
// ticket template tells a coder to read) plus its own (rework/continuation
// context). Like CLIPSE_ISSUE_TEXT/CLIPSE_REVIEW_FEEDBACK it is injected
// directly at spawn time, never sourced from the host environment, and only
// for coder-lane spawns. graphs/coder.py load_context folds it into the DAC
// prompt under a "Dependency notes" heading. Best-effort: a Linear fetch
// failure leaves it unset rather than failing the spawn.
const clipseDependencyNotesEnvVar = "CLIPSE_DEPENDENCY_NOTES"

// issueText composes the worker-facing task text for issue: its title, plus
// a blank-line-separated description when non-empty (just the title
// otherwise). Trailing whitespace is stripped so a Linear issue with a
// trailing newline in its description doesn't leak one into the env var's
// value.
func issueText(issue store.Issue) string {
	text := issue.Title
	if issue.Description != "" {
		text += "\n\n" + issue.Description
	}
	return strings.TrimRightFunc(text, unicode.IsSpace)
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
	if err := d.drainWorkspaceCleanup(ctx); err != nil {
		return fmt.Errorf("tick: drain workspace cleanup: %w", err)
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
