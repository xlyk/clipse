package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/xlyk/clipse/internal/store"
)

func TestDispatcherControl_DefaultPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clipse.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	control, err := s.ReadDispatcherControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingRunning || control.ObservedMode != store.ObservedRunning {
		t.Fatalf("default modes = %s/%s, want running/running", control.DesiredMode, control.ObservedMode)
	}
	if err := s.RequestPause(context.Background(), "pause-1", 100); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	control, err = s.ReadDispatcherControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingPaused || control.RequestID != "pause-1" || control.RequestedAt != 100 {
		t.Fatalf("reopened control = %+v, want durable pause request", control)
	}
}

func TestDispatcherControl_RegisterHeartbeatDrainAndResume(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	registration, err := s.RegisterDispatcher(ctx, "instance-a", 1234, 100)
	if err != nil {
		t.Fatal(err)
	}
	if registration.DrainInterrupted {
		t.Fatal("fresh registration unexpectedly interrupted a drain")
	}
	if err := s.RequestDrain(ctx, "drain-1", 110, true); err != nil {
		t.Fatal(err)
	}
	control, err := s.ReadDispatcherControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.DrainTargetInstanceID != "instance-a" || !control.DrainStrict || control.DesiredMode != store.SchedulingPaused {
		t.Fatalf("drain request = %+v", control)
	}
	if err := s.HeartbeatDispatcher(ctx, "instance-a", store.ObservedDraining, 120); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestResume(ctx, "resume-refused", 125, false); !errors.Is(err, store.ErrDrainInProgress) {
		t.Fatalf("plain resume during drain = %v, want ErrDrainInProgress", err)
	}
	if err := s.CompleteDrain(ctx, "instance-a", 130); err != nil {
		t.Fatal(err)
	}
	control, err = s.ReadDispatcherControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.ObservedMode != store.ObservedPaused || control.DrainedAt != 130 || control.DrainTargetInstanceID != "" || control.ActiveInstanceID != "" {
		t.Fatalf("completed drain = %+v", control)
	}
	if err := s.RequestResume(ctx, "resume-1", 140, false); err != nil {
		t.Fatal(err)
	}
	control, err = s.ReadDispatcherControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingRunning || control.RequestID != "resume-1" {
		t.Fatalf("resumed control = %+v", control)
	}
}

func TestDispatcherControl_NewInstanceInterruptsStaleDrainAndStaysPaused(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.RegisterDispatcher(ctx, "old", 111, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestDrain(ctx, "drain-1", 20, false); err != nil {
		t.Fatal(err)
	}
	registration, err := s.RegisterDispatcher(ctx, "replacement", 222, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !registration.DrainInterrupted {
		t.Fatal("replacement registration did not report interrupted drain")
	}
	if registration.Control.DesiredMode != store.SchedulingPaused || registration.Control.ObservedMode != store.ObservedPaused {
		t.Fatalf("replacement modes = %s/%s, want paused/paused", registration.Control.DesiredMode, registration.Control.ObservedMode)
	}
	if registration.Control.DrainTargetInstanceID != "" {
		t.Fatalf("stale drain target = %q, want cleared", registration.Control.DrainTargetInstanceID)
	}
	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Kind; got != "dispatcher_drain_interrupted" {
		t.Fatalf("last event = %q, want dispatcher_drain_interrupted", got)
	}
}

func TestSchedulingPauseGatesReadyAndColumnClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	seedReadyIssue(t, s, "ready-1", "coder", 1, 10)
	seedColumnIssue(t, s, "review-1", "review", 1, 20)
	if err := s.RequestPause(ctx, "pause-1", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimReady(ctx, "coder", "run-ready", 40, 60); !errors.Is(err, store.ErrSchedulingPaused) {
		t.Fatalf("ClaimReady = %v, want ErrSchedulingPaused", err)
	}
	if _, err := s.ClaimColumn(ctx, "review", "reviewer", "run-review", 40, 60); !errors.Is(err, store.ErrSchedulingPaused) {
		t.Fatalf("ClaimColumn = %v, want ErrSchedulingPaused", err)
	}
	runs, err := s.ListOpenRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("open runs = %d, want zero", len(runs))
	}
}

func TestSchedulingPauseCommitIsBarrierForConcurrentClaims(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		seedReadyIssue(t, s, "issue-"+string(rune('a'+i)), "coder", 1, int64(i))
	}

	start := make(chan struct{})
	var prePauseWins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if _, err := s.ClaimReady(ctx, "coder", "race-"+string(rune('a'+i)), 100, 60); err == nil {
				prePauseWins.Add(1)
			} else if !errors.Is(err, store.ErrSchedulingPaused) && !errors.Is(err, store.ErrNoReady) {
				t.Errorf("racing claim: %v", err)
			}
		}(i)
	}
	close(start)
	if err := s.RequestPause(ctx, "pause-race", 101); err != nil {
		t.Fatal(err)
	}

	// The pause transaction has committed. Every transaction begun from this
	// point must observe it, regardless of which side won the earlier race.
	for i := 0; i < 20; i++ {
		if _, err := s.ClaimReady(ctx, "coder", "post-pause-"+string(rune('a'+i)), 102, 60); !errors.Is(err, store.ErrSchedulingPaused) {
			t.Fatalf("post-pause claim %d = %v, want ErrSchedulingPaused", i, err)
		}
	}
	wg.Wait()
	if prePauseWins.Load() > 20 {
		t.Fatalf("impossible pre-pause win count %d", prePauseWins.Load())
	}
}
