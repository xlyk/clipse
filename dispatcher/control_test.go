package dispatcher_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

func singleCoderConfig() config.Config {
	cfg := testConfig()
	cfg.Caps.Global = 1
	cfg.Caps.PerLane.Coder = 1
	return cfg
}

func TestTick_PausedReconcilesActiveResultWithoutClaimingQueuedWork(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	seedReadyIssue(t, s, "issue-2", "coder", 1, 20)
	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{Worker: contract.WorkerResult{
		Outcome: contract.WorkerResultOutcomeNeedsReview,
		Summary: "ready for review",
	}}
	d := dispatcher.New(singleCoderConfig(), s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)
	ctx := context.Background()
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestPause(ctx, "pause-1", 1001); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		open, err := s.ListOpenRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(open) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	first, err := s.GetIssue(ctx, "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetIssue(ctx, "issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if first.BoardStatus != "review" || first.ClaimLock.Valid {
		t.Fatalf("active issue after paused reconciliation = %+v, want unclaimed review", first)
	}
	if second.BoardStatus != "ready" || second.ClaimLock.Valid {
		t.Fatalf("queued issue while paused = %+v, want unclaimed ready", second)
	}
	if spawner.SpawnCount() != 1 {
		t.Fatalf("spawn count = %d, want 1", spawner.SpawnCount())
	}
}

func TestRun_TargetedDrainWaitsForActiveWorkerThenExitsPaused(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	seedReadyIssue(t, s, "issue-2", "coder", 1, 20)
	gate := &gatedSpawner{
		release: make(chan struct{}),
		result: spawn.Result{Worker: contract.WorkerResult{
			Outcome: contract.WorkerResultOutcomeNeedsReview,
			Summary: "ready for review",
		}},
	}
	d := dispatcher.New(singleCoderConfig(), s, &linear.MockClient{}, gate, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond),
		dispatcher.WithInstanceID("instance-a"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	waitFor(t, 2*time.Second, func() bool {
		control, err := s.ReadDispatcherControl(context.Background())
		return err == nil && control.ActiveInstanceID == "instance-a" && gate.spawnedCount() == 1
	}, "dispatcher registration and first spawn")
	if err := s.RequestDrain(context.Background(), "drain-1", 1100, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool {
		control, err := s.ReadDispatcherControl(context.Background())
		return err == nil && control.ObservedMode == store.ObservedDraining
	}, "drain acknowledgment")
	if gate.spawnedCount() != 1 {
		t.Fatalf("drain allowed another spawn before release: %d", gate.spawnedCount())
	}
	close(gate.release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after active worker reached a durable result")
	}
	control, err := s.ReadDispatcherControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingPaused || control.ObservedMode != store.ObservedPaused || control.DrainedAt == 0 || control.ActiveInstanceID != "" {
		t.Fatalf("completed control = %+v", control)
	}
	if gate.spawnedCount() != 1 {
		t.Fatalf("spawn count = %d, want 1", gate.spawnedCount())
	}
	open, err := s.ListOpenRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("open runs after drain = %d, want zero", len(open))
	}
	second, err := s.GetIssue(context.Background(), "issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimLock.Valid || second.BoardStatus != "ready" {
		t.Fatalf("queued issue after drain = %+v", second)
	}
}

func TestRun_DrainAllowsSameClaimContinuation(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	seedReadyIssue(t, s, "issue-2", "coder", 1, 20)
	spawner := newFakeSpawner()
	spawner.ResultsQueue["issue-1"] = []spawn.Result{
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeContinue, Summary: "continue"}},
		{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeNeedsReview, Summary: "ready"}},
	}
	d := dispatcher.New(singleCoderConfig(), s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(100*time.Millisecond),
		dispatcher.WithInstanceID("instance-a"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return spawner.SpawnCount() == 1 }, "first coder turn")
	if err := s.RequestDrain(context.Background(), "drain-continue", 1100, false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not complete continuation drain")
	}
	if spawner.SpawnCount() != 2 {
		t.Fatalf("spawn count = %d, want two turns under one admitted claim", spawner.SpawnCount())
	}
	second, err := s.GetIssue(context.Background(), "issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.ClaimLock.Valid {
		t.Fatalf("queued issue claimed during drain: %+v", second)
	}
}

func TestRun_NonStrictDrainCompletesWithPendingOutbox(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	gate := &gatedSpawner{
		release: make(chan struct{}),
		result: spawn.Result{Worker: contract.WorkerResult{
			Outcome: contract.WorkerResultOutcomeNeedsReview,
			Summary: "ready",
		}},
	}
	lc := &linear.MockClient{SetStateErr: errFakeLinearDown}
	d := dispatcher.New(singleCoderConfig(), s, lc, gate, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond), dispatcher.WithInstanceID("instance-a"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return gate.spawnedCount() == 1 }, "outbox test worker")
	if err := s.RequestDrain(context.Background(), "drain-outbox", 1100, false); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-strict drain was blocked by pending Linear writes")
	}
	counts, err := s.DispatcherRuntimeCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.PendingOutbox == 0 {
		t.Fatal("non-strict drain test did not retain a pending outbox row")
	}
}

func TestRun_StrictDrainWaitsForPendingOutbox(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	gate := &gatedSpawner{
		release: make(chan struct{}),
		result: spawn.Result{Worker: contract.WorkerResult{
			Outcome: contract.WorkerResultOutcomeNeedsReview,
			Summary: "ready",
		}},
	}
	d := dispatcher.New(singleCoderConfig(), s, &linear.MockClient{SetStateErr: errFakeLinearDown}, gate, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond), dispatcher.WithInstanceID("instance-a"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return gate.spawnedCount() == 1 }, "strict outbox test worker")
	if err := s.RequestDrain(context.Background(), "drain-strict", 1100, true); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	waitFor(t, 2*time.Second, func() bool {
		counts, err := s.DispatcherRuntimeCounts(context.Background())
		control, controlErr := s.ReadDispatcherControl(context.Background())
		return err == nil && controlErr == nil && counts.ActiveRuns == 0 && counts.PendingOutbox > 0 && control.ObservedMode == store.ObservedDraining
	}, "strict drain durable backlog")
	select {
	case err := <-done:
		t.Fatalf("strict drain exited with pending outbox: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("strict drain test dispatcher did not stop after cancellation")
	}
}

func TestRun_DrainPersistsTransientRetryWithoutReclaiming(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 10)
	cfg := singleCoderConfig()
	cfg.RecoverCap = 1
	cfg.RecoverBackoffS = 0
	gate := &gatedSpawner{
		release: make(chan struct{}),
		result:  spawn.Result{Err: fmt.Errorf("worker crash: %w", spawn.ErrWorkerExit)},
	}
	d := dispatcher.New(cfg, s, &linear.MockClient{}, gate, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithPollInterval(10*time.Millisecond), dispatcher.WithInstanceID("instance-a"),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	waitFor(t, 2*time.Second, func() bool { return gate.spawnedCount() == 1 }, "transient worker")
	if err := s.RequestDrain(context.Background(), "drain-retry", 1100, false); err != nil {
		t.Fatal(err)
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not complete after transient result became durable")
	}
	issue, err := s.GetIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if issue.BoardStatus != "ready" || issue.RecoverAttempts != 1 || issue.ClaimLock.Valid {
		t.Fatalf("durable retry = %+v, want unclaimed ready with one recovery attempt", issue)
	}
	if gate.spawnedCount() != 1 {
		t.Fatalf("retry was reclaimed during drain: spawn count %d", gate.spawnedCount())
	}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
