package spawn

import (
	"errors"
	"fmt"
	"syscall"
)

// Reaped describes the outcome of a ReapOrphan call.
type Reaped int

const (
	// ReapedKilled means a live process matching expectedProcStartedAt was
	// found and its process group was killed.
	ReapedKilled Reaped = iota

	// ReapedAlreadyGone means there was no live process at pid (or pid was
	// never a real process id, e.g. <= 0), so nothing needed killing.
	ReapedAlreadyGone

	// ReapedIdentityMismatch means pid is alive, but its actual process
	// start time does not match expectedProcStartedAt: the OS has reused
	// pid for an unrelated process since the run this pid was recorded for.
	// The live process is intentionally left alone.
	ReapedIdentityMismatch
)

// String renders a Reaped value for logging.
func (r Reaped) String() string {
	switch r {
	case ReapedKilled:
		return "killed"
	case ReapedAlreadyGone:
		return "already_gone"
	case ReapedIdentityMismatch:
		return "identity_mismatch"
	default:
		return fmt.Sprintf("Reaped(%d)", int(r))
	}
}

// ReapOrphan checks whether pid is a live process matching
// expectedProcStartedAt (the process identity recorded via SetRunProcess
// when the worker was spawned) and, if so, kills its process group. This is
// the dispatcher-restart orphan-recovery primitive (A1): a run left
// status='running' by a dispatcher process that died is only safe to treat
// as dead once we've verified the OS hasn't already reused its pid for an
// unrelated process.
//
// expectedProcStartedAt <= 0 means the original spawn's start time was never
// recorded (or was unverifiable — see processStartTime); in that case the
// identity check is skipped and a live pid is killed unconditionally, since
// there is nothing to compare against.
//
// Workers are always spawned as their own process-group leader (Setpgid,
// see LocalSpawner.Spawn), so pid is also its process group id: killing
// signals the whole group, catching any children the worker spawned, not
// just the leader.
func ReapOrphan(pid int, expectedProcStartedAt int64) (Reaped, error) {
	if pid <= 0 {
		return ReapedAlreadyGone, nil
	}

	// Signal 0 sends no signal but still performs the existence/permission
	// check, so this tells us whether pid is alive without disturbing it.
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ReapedAlreadyGone, nil
		}
		return ReapedAlreadyGone, fmt.Errorf("checking liveness of pid %d: %w", pid, err)
	}

	if expectedProcStartedAt > 0 {
		liveStartedAt, err := processStartTime(pid)
		if err != nil {
			// The process may have exited in the window between the Kill(0)
			// liveness check above and this ps call — that races as
			// AlreadyGone rather than a hard error, since the pid is
			// effectively gone either way.
			if procGone(pid) {
				return ReapedAlreadyGone, nil
			}
			return ReapedAlreadyGone, fmt.Errorf("reading start time of pid %d: %w", pid, err)
		}
		if liveStartedAt != expectedProcStartedAt {
			return ReapedIdentityMismatch, nil
		}
	}

	pgid := pid
	// Best-effort graceful termination first, exactly like localHandle.Kill:
	// ignore errors here since the process may already be gone or may
	// ignore SIGTERM, either of which the follow-up SIGKILL handles.
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			// Process group vanished between the liveness check and the
			// kill: treat as already gone rather than an error.
			return ReapedAlreadyGone, nil
		}
		return ReapedAlreadyGone, fmt.Errorf("killing orphaned process group %d: %w", pgid, err)
	}
	return ReapedKilled, nil
}

// procGone reports whether pid no longer exists, used to distinguish a
// genuine ps failure from a benign "the process exited while we were
// checking" race.
func procGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err != nil && errors.Is(err, syscall.ESRCH)
}
