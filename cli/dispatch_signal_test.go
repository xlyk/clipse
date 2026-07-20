package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/store"
)

type registeringRunner struct {
	store      *store.Store
	instanceID string
	ready      chan struct{}
}

func (r *registeringRunner) Run(ctx context.Context) error {
	if _, err := r.store.RegisterDispatcher(ctx, r.instanceID, os.Getpid(), 100); err != nil {
		return err
	}
	close(r.ready)
	<-ctx.Done()
	return r.store.UnregisterDispatcher(context.Background(), r.instanceID, 200)
}

func openSignalTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "clipse.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCoordinateDispatcherSignals_FirstSignalWithoutWorkExits(t *testing.T) {
	s := openSignalTestStore(t)
	runner := &registeringRunner{store: s, instanceID: "instance-a", ready: make(chan struct{})}
	signals := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		done <- coordinateDispatcherSignals(s, runner, slog.New(slog.NewTextHandler(io.Discard, nil)), signals)
	}()
	<-runner.ready
	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not exit after first signal with no active runs")
	}
	control, err := s.ReadDispatcherControl(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if control.DesiredMode != store.SchedulingRunning {
		t.Fatalf("idle signal changed desired mode to %s, want running", control.DesiredMode)
	}
}

func TestCoordinateDispatcherSignals_FirstDrainsSecondForces(t *testing.T) {
	s := openSignalTestStore(t)
	ctx := context.Background()
	if err := s.UpsertIssue(ctx, store.Issue{ID: "issue-1", Identifier: "CLP-1", LaneLabel: "coder", BoardStatus: "ready", Deps: "[]"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimReady(ctx, "coder", "run-1", 10, 100); err != nil {
		t.Fatal(err)
	}
	runner := &registeringRunner{store: s, instanceID: "instance-a", ready: make(chan struct{})}
	signals := make(chan os.Signal, 2)
	done := make(chan error, 1)
	go func() {
		done <- coordinateDispatcherSignals(s, runner, slog.New(slog.NewTextHandler(io.Discard, nil)), signals)
	}()
	<-runner.ready
	signals <- syscall.SIGTERM

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		control, err := s.ReadDispatcherControl(ctx)
		if err == nil && control.DesiredMode == store.SchedulingPaused && control.DrainTargetInstanceID == "instance-a" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	control, err := s.ReadDispatcherControl(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if control.DrainTargetInstanceID != "instance-a" {
		t.Fatalf("first signal control = %+v, want targeted drain", control)
	}
	select {
	case err := <-done:
		t.Fatalf("coordinator exited on first active-work signal: %v", err)
	default:
	}

	signals <- syscall.SIGTERM
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not force exit on second signal")
	}
}
