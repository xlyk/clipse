package dispatcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/linear"
)

// TestSpawnAttempt_DefaultEnvFor_FiltersToConfiguredAllowlist proves the
// production gap this task closes: constructed exactly the way
// cli/dispatch.go's runDispatch wires the real Dispatcher (no WithEnvFor
// override), a claimed issue's spawned worker gets an environment built
// ONLY from cfg.EnvAllowlist. A real LINEAR_API_KEY the dispatcher process
// holds (as production always does, per the design doc) must never reach
// the worker, while an allow-listed secret and the TESTWORKER_SCENARIO
// passthrough kernel tests rely on both still do.
func TestSpawnAttempt_DefaultEnvFor_FiltersToConfiguredAllowlist(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "kernel-only-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-worker-secret")
	t.Setenv("TESTWORKER_SCENARIO", "needs_review")
	t.Setenv("SOME_UNRELATED_VAR", "should-not-leak-either")

	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)

	lc := &linear.MockClient{}
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.EnvAllowlist = []string{"ANTHROPIC_API_KEY", "PATH", "TESTWORKER_SCENARIO"}

	// Deliberately NOT passing dispatcher.WithEnvFor: this must exercise
	// New's default, the same path production wiring uses.
	d := dispatcher.New(cfg, s, lc, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: unexpected error: %v", err)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("SpawnCount = %d, want exactly 1", len(specs))
	}

	got := make(map[string]string, len(specs[0].Env))
	for _, kv := range specs[0].Env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("worker env entry %q is not KEY=VALUE", kv)
		}
		if _, dup := got[key]; dup {
			t.Fatalf("worker env has duplicate key %q", key)
		}
		got[key] = value
	}

	for key := range got {
		if key != "ANTHROPIC_API_KEY" && key != "PATH" && key != "TESTWORKER_SCENARIO" {
			t.Errorf("worker env contains non-allowlisted key %q (env=%v)", key, specs[0].Env)
		}
	}
	if _, leaked := got["LINEAR_API_KEY"]; leaked {
		t.Errorf("worker env leaked LINEAR_API_KEY (env=%v)", specs[0].Env)
	}
	if _, leaked := got["SOME_UNRELATED_VAR"]; leaked {
		t.Errorf("worker env leaked SOME_UNRELATED_VAR (env=%v)", specs[0].Env)
	}
	if _, ok := got["PATH"]; !ok {
		t.Errorf("worker env missing PATH (env=%v)", specs[0].Env)
	}
	if v := got["ANTHROPIC_API_KEY"]; v != "sk-worker-secret" {
		t.Errorf("worker env ANTHROPIC_API_KEY = %q, want sk-worker-secret", v)
	}
	if v := got["TESTWORKER_SCENARIO"]; v != "needs_review" {
		t.Errorf("worker env TESTWORKER_SCENARIO = %q, want needs_review (kernel-test passthrough)", v)
	}
}
