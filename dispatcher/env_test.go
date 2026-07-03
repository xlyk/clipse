package dispatcher_test

import (
	"context"
	"strings"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// TestSpawnAttempt_DefaultEnvFor_FiltersToConfiguredAllowlist proves the
// production gap this task closes: constructed exactly the way
// cli/dispatch.go's runDispatch wires the real Dispatcher (no WithEnvFor
// override), a claimed issue's spawned worker gets an environment built from
// cfg.EnvAllowlist PLUS a dispatcher-computed CLIPSE_ISSUE_TEXT (the claimed
// issue's title/description -- see defaultEnvFor/issueText). A real
// LINEAR_API_KEY the dispatcher process holds (as production always does,
// per the design doc) must never reach the worker, while an allow-listed
// secret, the TESTWORKER_SCENARIO passthrough kernel tests rely on, and
// CLIPSE_ISSUE_TEXT itself all still do.
func TestSpawnAttempt_DefaultEnvFor_FiltersToConfiguredAllowlist(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "kernel-only-secret")
	t.Setenv("ANTHROPIC_API_KEY", "sk-worker-secret")
	t.Setenv("TESTWORKER_SCENARIO", "needs_review")
	t.Setenv("SOME_UNRELATED_VAR", "should-not-leak-either")

	s := openTestStore(t)
	ctx := context.Background()
	// Built directly (rather than via seedReadyIssue) so the claimed issue
	// carries title/description -- what CLIPSE_ISSUE_TEXT is computed from.
	if err := s.UpsertIssue(ctx, store.Issue{
		ID:          "issue-1",
		Identifier:  "issue-1",
		Title:       "Add the thing",
		Description: "Implement the thing that does the stuff.",
		LaneLabel:   "coder",
		BoardStatus: "ready",
		Deps:        `[]`,
		Priority:    1,
		BranchName:  "issue-1-branch",
		UpdatedAt:   100,
		LastSeen:    100,
		CreatedAt:   100,
	}); err != nil {
		t.Fatalf("seed UpsertIssue: unexpected error: %v", err)
	}

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

	if err := d.Tick(ctx); err != nil {
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
		if key != "ANTHROPIC_API_KEY" && key != "PATH" && key != "TESTWORKER_SCENARIO" && key != "CLIPSE_ISSUE_TEXT" {
			t.Errorf("worker env contains non-allowlisted key %q (env=%v)", key, specs[0].Env)
		}
	}
	if _, leaked := got["LINEAR_API_KEY"]; leaked {
		t.Errorf("worker env leaked LINEAR_API_KEY (env=%v)", specs[0].Env)
	}
	// A fresh ready claim is not a rework re-run, so it carries no review
	// feedback — only a coder claim out of the rework column does.
	if _, present := got["CLIPSE_REVIEW_FEEDBACK"]; present {
		t.Errorf("worker env has CLIPSE_REVIEW_FEEDBACK on a fresh ready claim (env=%v)", specs[0].Env)
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

	// The dispatcher-computed value this task adds: present regardless of
	// the allow-list, built from the claimed issue's title/description.
	wantIssueText := "Add the thing\n\nImplement the thing that does the stuff."
	if v, ok := got["CLIPSE_ISSUE_TEXT"]; !ok {
		t.Errorf("worker env missing CLIPSE_ISSUE_TEXT (env=%v)", specs[0].Env)
	} else if v != wantIssueText {
		t.Errorf("worker env CLIPSE_ISSUE_TEXT = %q, want %q", v, wantIssueText)
	}
}
