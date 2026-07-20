package dispatcher_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
)

// TestRun_RecoversOrphansOnceBeforeLooping asserts Run calls RecoverOrphans
// exactly once, before Tick starts looping: a running issue seeded to mimic
// a dead dispatcher's leftovers must be requeued shortly after Run starts,
// without waiting for the orphan's own claim TTL to expire.
//
// It asserts via the "orphan_requeue" event rather than sampling
// issue.BoardStatus == ready directly, because with a 10ms poll interval a
// later Tick can re-claim (and even spawn/block) the requeued issue again
// before a polling goroutine samples the transient ready state — the event
// log entry, in contrast, is a permanent, exactly-once record of the
// original RecoverOrphans requeue regardless of what happens afterward.
func TestRun_RecoversOrphansOnceBeforeLooping(t *testing.T) {
	s := openTestStore(t)
	boardDir := t.TempDir()

	handle := spawnRealOrphan(t, boardDir, "issue-1", "coder")
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()
	seedOrphanRun(t, s, "issue-1", "coder", "orphan-run", pid, startedAt, 1, 1000)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(2000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	var requeueCount int
	for time.Now().Before(deadline) {
		events, err := s.ListEvents(context.Background())
		if err != nil {
			t.Fatalf("ListEvents: unexpected error: %v", err)
		}
		requeueCount = 0
		for _, e := range events {
			if e.Kind == "orphan_requeue" && e.RunID.String == "orphan-run" {
				requeueCount++
			}
		}
		if requeueCount > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s of ctx cancellation")
	}

	_, _ = handle.Wait()

	if requeueCount != 1 {
		t.Fatalf("orphan_requeue events for run orphan-run = %d, want exactly 1 (RecoverOrphans must run exactly once, before the loop)", requeueCount)
	}
}

// TestRun_TicksRepeatedlyOnPollInterval asserts Run invokes Tick immediately
// on start and then again on every WithPollInterval tick, by counting
// CandidateIssues calls (Tick's poll phase calls it once per Tick).
func TestRun_TicksRepeatedlyOnPollInterval(t *testing.T) {
	s := openTestStore(t)
	lc := &countingLinearClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lc.Count() >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s of ctx cancellation")
	}

	if got := lc.Count(); got < 3 {
		t.Fatalf("CandidateIssues call count = %d, want >= 3 (immediate tick + at least 2 interval ticks)", got)
	}
}

// TestRun_ReturnsPromptlyOnCtxCancel asserts Run returns quickly (well under
// a full poll interval) once ctx is cancelled, rather than blocking for the
// remainder of the current interval.
func TestRun_ReturnsPromptlyOnCtxCancel(t *testing.T) {
	s := openTestStore(t)
	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(1*time.Hour), // long enough that only cancellation ends Run
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Let Run get through its immediate Tick + RecoverOrphans before cancel.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
		if elapsed := time.Since(start); elapsed > 1*time.Second {
			t.Errorf("Run took %v to return after cancel, want < 1s", elapsed)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s of ctx cancellation")
	}
}

// TestRun_GracefulShutdownDoesNotKillLiveWorker is the core no-kill
// assertion: a worker spawned during a Tick must NOT be cancelled/killed
// when Run's ctx is cancelled. It uses a spawner double whose handle blocks
// in Wait() until explicitly released, so if Run (transitively, via Tick's
// spawnAttempt) rooted the worker's context at the cancellable Run ctx
// instead of context.WithoutCancel(ctx), the gate goroutine below would
// observe ctx.Done() firing before the test releases it.
func TestRun_GracefulShutdownDoesNotKillLiveWorker(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	gate := &gatedSpawner{release: make(chan struct{})}
	ws := newStubWorkspacer(t.TempDir())
	lc := &linear.MockClient{}
	cfg := testConfig()
	cfg.MaxRuntimeS = 3600 // long enough that only explicit release ends Wait
	d := dispatcher.New(cfg, s, lc, gate, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	// Wait for the worker to be spawned.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gate.spawnedCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if gate.spawnedCount() == 0 {
		t.Fatal("gatedSpawner.Spawn was never called")
	}

	// Cancel Run's ctx (simulating SIGINT) while the worker is still live.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Run did not return within 1s of ctx cancellation")
	}

	// The worker's spawn context must still be alive after Run has returned:
	// shutdown must not have cancelled it. Only releasing the gate ends Wait.
	if gate.handleCtxErr() != nil {
		t.Errorf("worker's spawn ctx.Err() = %v after Run returned, want nil (shutdown must not cancel live workers)", gate.handleCtxErr())
	}

	close(gate.release)
	if _, err := gate.lastHandle().Wait(); err != nil {
		t.Errorf("Wait() after release: unexpected error: %v", err)
	}
}

// countingLinearClient wraps linear.MockClient's zero-issue behavior with an
// atomic call counter, so tests can assert Tick ran a specific number of
// times without depending on wall-clock sleeps for correctness.
type countingLinearClient struct {
	linear.MockClient
	n atomic.Int64
}

func (c *countingLinearClient) CandidateIssues(ctx context.Context) ([]linear.Issue, error) {
	c.n.Add(1)
	return c.MockClient.CandidateIssues(ctx)
}

func (c *countingLinearClient) Count() int64 { return c.n.Load() }

// gatedSpawner is a Spawner test double whose single handle's Wait() blocks
// on a caller-controlled channel, so a test can hold a "worker" open across
// a Run ctx cancellation and observe whether its context got cancelled.
type gatedSpawner struct {
	release chan struct{}
	result  spawn.Result

	n      atomic.Int64
	handle *gatedHandle
}

func (g *gatedSpawner) Spawn(ctx context.Context, spec spawn.WorkerSpec) (spawn.RunHandle, error) {
	g.n.Add(1)
	result := g.result
	if result.Worker.Outcome == "" && result.Err == nil {
		result = spawn.Result{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeDone}}
	}
	h := &gatedHandle{ctx: ctx, release: g.release, result: result, pid: 12345}
	g.handle = h
	return h, nil
}

func (g *gatedSpawner) spawnedCount() int64 { return g.n.Load() }

func (g *gatedSpawner) lastHandle() *gatedHandle { return g.handle }

func (g *gatedSpawner) handleCtxErr() error {
	if g.handle == nil {
		return nil
	}
	return g.handle.ctx.Err()
}

type gatedHandle struct {
	ctx     context.Context
	release chan struct{}
	result  spawn.Result
	pid     int
}

func (h *gatedHandle) PID() int             { return h.pid }
func (h *gatedHandle) ProcStartedAt() int64 { return 1000 }
func (h *gatedHandle) Kill() error          { return nil }

func (h *gatedHandle) Wait() (spawn.Result, error) {
	<-h.release
	return h.result, nil
}
