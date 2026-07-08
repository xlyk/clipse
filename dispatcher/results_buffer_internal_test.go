package dispatcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/store"
)

// TestNew_SizesResultsBufferAtLeastCapsGlobalPlusOne asserts the fix for the
// Tick self-send deadlock's first layer of defense: d.results must always
// have room for one result per concurrently-running worker (cfg.Caps.Global)
// plus one spare slot, so applyResult's GetIssue-failure requeue (the send at
// reconcile.go's applyResult) never has to contend with every lane-cap slot
// already occupying the buffer. Before this fix, the buffer was pinned at
// defaultResultsBuffer regardless of cfg.Caps.Global -- fine while Caps.Global
// stayed comfortably under 256, but a deploy configured with a larger global
// cap could fill the buffer with legitimate in-flight results alone, leaving
// no room for a same-tick requeue.
func TestNew_SizesResultsBufferAtLeastCapsGlobalPlusOne(t *testing.T) {
	tests := []struct {
		name       string
		capsGlobal int
		want       int
	}{
		{"small global cap stays at the default floor", 8, defaultResultsBuffer},
		{"global cap above the default grows the buffer", defaultResultsBuffer + 10, defaultResultsBuffer + 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Config{Caps: config.Caps{Global: tt.capsGlobal}}
			d := New(cfg, nil, nil, nil, nil)
			if got := cap(d.results); got != tt.want {
				t.Errorf("cap(d.results) = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestApplyResult_GetIssueFailureFallsBackWhenResultsChannelFull asserts the
// fix for the Tick self-send deadlock's second layer of defense: applyResult
// (dispatcher/reconcile.go) requeues a result it can't yet apply by sending
// rr back onto d.results -- the exact channel Tick's own drainResults loop is
// the ONLY reader of. A plain blocking send there is a deadlock waiting to
// happen: if the buffer is ever full, the send blocks on the very goroutine
// that would otherwise drain it, forever. The fix makes that send
// non-blocking with a documented fallback: leave the record in d.inflight
// (still heartbeat-renewed every tick, so the run stays visible) instead of
// either blocking forever or silently dropping the result.
func TestApplyResult_GetIssueFailureFallsBackWhenResultsChannelFull(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "clipse.db"))
	if err != nil {
		t.Fatalf("store.Open: unexpected error: %v", err)
	}
	defer s.Close()

	d := New(config.Config{Caps: config.Caps{Global: 1}}, s, nil, nil, nil, WithResultsBuffer(1))

	// Fill the buffer to capacity with an unrelated placeholder result, so
	// the requeue send below finds no room.
	d.results <- runResult{runID: "filler", issueID: "filler"}

	// issue-missing was never inserted, so GetIssue fails -- the exact
	// condition applyResult's requeue branch exists for (Dispatcher.store is
	// a concrete *store.Store with no interface seam to mock a targeted
	// failure; a missing row is the simplest real GetIssue failure).
	d.inflight["run-1"] = inflightRun{issueID: "issue-missing", cancel: func() {}}
	rr := runResult{runID: "run-1", issueID: "issue-missing"}

	done := make(chan error, 1)
	go func() {
		done <- d.applyResult(context.Background(), rr)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("applyResult: want a wrapped GetIssue error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("applyResult blocked on a full results channel -- the Tick self-send deadlock")
	}

	if _, ok := d.inflight["run-1"]; !ok {
		t.Error(`d.inflight["run-1"] missing, want it left in place so the run stays visible for a later retry`)
	}
	if len(d.results) != 1 {
		t.Errorf("len(d.results) = %d, want 1 (only the original filler -- rr must not have been silently dropped, nor was there room to requeue it)", len(d.results))
	}
}
