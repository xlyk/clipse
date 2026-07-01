package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// errFakeLinearDown is a scripted Linear outage error for MockClient's
// SetStateErr/CommentErr, used to drive the outbox retry path.
var errFakeLinearDown = errors.New("fake linear outage")

// fakeSpawner is a Spawner test double: Spawn returns a fakeHandle whose
// Wait() result is looked up from a caller-scripted table keyed by
// spec.Issue — the Linear identifier (e.g. "CLP-1"), NOT the store's
// internal issue id, since that's what WorkerSpec.Issue is built from (see
// dispatcher/schedule.go's spawnClaim / spawnAttempt). Tests must key
// Results/ResultsQueue by whatever Identifier the seeded/polled issue ends
// up with, which is often the same string as its store id in these tests
// but is NOT guaranteed to be (a poll overwrites Identifier from Linear).
//
// Results can be scripted once (a single Result reused for every spawn of
// that issue) or as a queue (consumed one per spawn, for continuation tests
// that need a different outcome each turn). ResultsQueue takes priority over
// Results for a given issue when both are set.
type fakeSpawner struct {
	mu sync.Mutex

	// Results maps WorkerSpec.Issue (the Linear identifier) -> the Result
	// returned for every Spawn of that issue (unless ResultsQueue has
	// entries left for it).
	Results map[string]spawn.Result

	// ResultsQueue maps WorkerSpec.Issue (the Linear identifier) -> a queue
	// of Results, consumed (FIFO) one per Spawn call for that issue.
	ResultsQueue map[string][]spawn.Result

	// SpawnErr, if set, is returned by Spawn instead of a handle (for every
	// issue), simulating a Spawner-level failure (not a worker failure).
	SpawnErr error

	spawnCount int32
	specs      []spawn.WorkerSpec
}

func newFakeSpawner() *fakeSpawner {
	return &fakeSpawner{
		Results:      make(map[string]spawn.Result),
		ResultsQueue: make(map[string][]spawn.Result),
	}
}

func (f *fakeSpawner) Spawn(ctx context.Context, spec spawn.WorkerSpec) (spawn.RunHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	atomic.AddInt32(&f.spawnCount, 1)
	f.specs = append(f.specs, spec)

	if f.SpawnErr != nil {
		return nil, f.SpawnErr
	}

	var res spawn.Result
	if q := f.ResultsQueue[spec.Issue]; len(q) > 0 {
		res = q[0]
		f.ResultsQueue[spec.Issue] = q[1:]
	} else if r, ok := f.Results[spec.Issue]; ok {
		res = r
	} else {
		res = spawn.Result{Worker: contract.WorkerResult{Outcome: contract.WorkerResultOutcomeDone}}
	}

	return &fakeHandle{ctx: ctx, res: res, pid: int(atomic.AddInt32(&f.spawnCount, 0)) + 1000}, nil
}

func (f *fakeSpawner) SpawnCount() int {
	return int(atomic.LoadInt32(&f.spawnCount))
}

func (f *fakeSpawner) Specs() []spawn.WorkerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]spawn.WorkerSpec, len(f.specs))
	copy(out, f.specs)
	return out
}

// fakeHandle is a RunHandle test double. Wait blocks until ctx is done or
// returns immediately with the scripted Result — whichever the caller wants
// is controlled entirely by whether res.Err is context.DeadlineExceeded
// (tests scripting a timeout scenario pass a ctx-observing Result and rely
// on the caller's own context.WithTimeout to fire).
type fakeHandle struct {
	ctx context.Context
	res spawn.Result
	pid int

	killed atomic.Bool
}

func (h *fakeHandle) PID() int             { return h.pid }
func (h *fakeHandle) ProcStartedAt() int64 { return 1000 }

func (h *fakeHandle) Kill() error {
	h.killed.Store(true)
	return nil
}

// Wait honors ctx's deadline: if the scripted result wants to simulate a
// timeout, it blocks until ctx.Done() fires and reports
// context.DeadlineExceeded, exactly like the real LocalSpawner does when the
// spawn context's deadline expires while the worker is still running.
func (h *fakeHandle) Wait() (spawn.Result, error) {
	if h.res.Err == context.DeadlineExceeded {
		<-h.ctx.Done()
		return spawn.Result{Err: fmt.Errorf("worker killed after context deadline: %w", context.DeadlineExceeded)}, nil
	}
	return h.res, nil
}

// stubWorkspacer is a Workspacer test double: Ensure returns a fixed root
// dir (a fresh subdirectory per issue, so tests can assert distinct
// workspaces), Remove just records the call.
type stubWorkspacer struct {
	mu   sync.Mutex
	root string

	removed []string
}

func newStubWorkspacer(root string) *stubWorkspacer {
	return &stubWorkspacer{root: root}
}

func (w *stubWorkspacer) Ensure(issue store.Issue) (string, error) {
	return w.root + "/" + issue.ID, nil
}

func (w *stubWorkspacer) Remove(issue store.Issue) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removed = append(w.removed, issue.ID)
	return nil
}

// sequentialRunIDs returns a deterministic run id generator: "run-1",
// "run-2", ... in call order, safe for concurrent use even though the
// dispatcher itself only ever calls newRunID from the Tick goroutine.
func sequentialRunIDs() func() string {
	var n int32
	return func() string {
		return fmt.Sprintf("run-%d", atomic.AddInt32(&n, 1))
	}
}
