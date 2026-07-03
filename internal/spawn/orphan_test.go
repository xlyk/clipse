package spawn_test

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/spawn"
)

// TestReapOrphan_KillsLiveMatchingProcess asserts that a real, live
// group-leader process (spawned the same way the dispatcher spawns workers)
// whose identity (proc_started_at) matches the caller's expectation is
// killed, and ReapOrphan reports ReapedKilled.
func TestReapOrphan_KillsLiveMatchingProcess(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	spec := newSpec(t, boardDir, "hang")
	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	pid := handle.PID()
	startedAt := handle.ProcStartedAt()
	if startedAt <= 0 {
		t.Fatalf("precondition: ProcStartedAt() = %d, want > 0", startedAt)
	}

	// Give the process group leader a moment to fully establish (Setpgid is
	// synchronous in Start, but be defensive against scheduling flakiness).
	time.Sleep(100 * time.Millisecond)

	outcome, err := spawn.ReapOrphan(pid, startedAt)
	if err != nil {
		t.Fatalf("ReapOrphan: unexpected error: %v", err)
	}
	if outcome != spawn.ReapedKilled {
		t.Errorf("outcome = %v, want ReapedKilled", outcome)
	}

	// ReapOrphan signals the process group but does not itself wait() on it
	// (that is the Spawner/RunHandle's job, same as any other Kill path) —
	// reap it here via the handle so the OS-level pid is actually freed,
	// exactly as the dispatcher's own Wait-goroutine would once it observes
	// the kill. Without this, the killed process stays a zombie (still
	// answering kill(pid, 0)) rather than truly gone.
	_, _ = handle.Wait()

	if !processGone(t, pid) {
		t.Errorf("process %d still alive after ReapOrphan", pid)
	}
}

// TestReapOrphan_AlreadyGoneForDeadPID asserts a pid with no live process
// (never existed, or already exited) is reported as ReapedAlreadyGone
// without error.
func TestReapOrphan_AlreadyGoneForDeadPID(t *testing.T) {
	// A pid vanishingly unlikely to be alive/reused during the test run.
	const bogusPID = 999999

	outcome, err := spawn.ReapOrphan(bogusPID, 12345)
	if err != nil {
		t.Fatalf("ReapOrphan: unexpected error: %v", err)
	}
	if outcome != spawn.ReapedAlreadyGone {
		t.Errorf("outcome = %v, want ReapedAlreadyGone", outcome)
	}
}

// TestReapOrphan_ZeroOrNegativePIDIsAlreadyGone asserts a pid<=0 (e.g. a run
// that was never assigned a worker_pid) is treated as ReapedAlreadyGone.
func TestReapOrphan_ZeroOrNegativePIDIsAlreadyGone(t *testing.T) {
	outcome, err := spawn.ReapOrphan(0, 0)
	if err != nil {
		t.Fatalf("ReapOrphan(0, 0): unexpected error: %v", err)
	}
	if outcome != spawn.ReapedAlreadyGone {
		t.Errorf("outcome = %v, want ReapedAlreadyGone", outcome)
	}
}

// TestReapOrphan_IdentityMismatchDoesNotKill asserts that when the live
// process's actual start time differs from the caller's expectation (a
// different process reused the pid), ReapOrphan reports
// ReapedIdentityMismatch and leaves the live process running.
func TestReapOrphan_IdentityMismatchDoesNotKill(t *testing.T) {
	bin := buildTestworker(t)
	boardDir := t.TempDir()
	s := spawn.NewLocalSpawner([]string{bin}, boardDir)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	spec := newSpec(t, boardDir, "hang")
	handle, err := s.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: unexpected error: %v", err)
	}
	pid := handle.PID()
	t.Cleanup(func() { _ = handle.Kill() })

	time.Sleep(100 * time.Millisecond)

	// Deliberately wrong expected start time: guaranteed not to match the
	// live process's real start time.
	const wrongStartedAt int64 = 1
	outcome, err := spawn.ReapOrphan(pid, wrongStartedAt)
	if err != nil {
		t.Fatalf("ReapOrphan: unexpected error: %v", err)
	}
	if outcome != spawn.ReapedIdentityMismatch {
		t.Errorf("outcome = %v, want ReapedIdentityMismatch", outcome)
	}
	if processGone(t, pid) {
		t.Errorf("process %d was killed, want it left alone on identity mismatch", pid)
	}
}

// processGone reports whether pid no longer exists (signal 0 fails with
// ESRCH), polling briefly since SIGKILL delivery is not instantaneous.
func processGone(t *testing.T, pid int) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}
