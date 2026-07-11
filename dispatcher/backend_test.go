package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

type fakeBackendManager struct {
	workspace backend.Workspace
	err       error
	ensures   []backend.EnsureRequest
}

func (f *fakeBackendManager) Ensure(_ context.Context, request backend.EnsureRequest) (backend.Workspace, error) {
	f.ensures = append(f.ensures, request)
	return f.workspace, f.err
}

func (f *fakeBackendManager) Delete(context.Context, backend.Workspace) error { return nil }

func (f *fakeBackendManager) List(context.Context, backend.ListRequest) ([]backend.Workspace, error) {
	return nil, nil
}

func TestSpawnAttempt_DaytonaProvisionsPersistsAndSanitizesWorkerSpec(t *testing.T) {
	t.Setenv("PATH", "/host/bin")
	t.Setenv("HOME", "/host/home")
	t.Setenv("DAYTONA_API_KEY", "daytona-secret")
	t.Setenv("DAYTONA_API_URL", "https://daytona.example")
	t.Setenv("DAYTONA_TARGET", "us")
	t.Setenv("LINEAR_API_KEY", "kernel-secret")

	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	manager := &fakeBackendManager{workspace: backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:issue-1", ExternalID: "sandbox-1",
		WorkspacePath: "/home/daytona/workspace/clipse", State: "active",
	}}
	cfg := testConfig()
	cfg.AgentBackend = config.AgentBackend{Type: "daytona", Daytona: config.DaytonaBackend{
		AutoStopMinutes: 60, ReviewerAutoDeleteMinutes: 30, Snapshot: "snapshot-1", Target: "us",
	}}
	cfg.Repo.Remote = "git@github.com:xlyk/clipse.git"

	d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithBackendManager(manager),
		dispatcher.WithEnvFor(func(store.Issue) []string {
			return []string{
				"ANTHROPIC_API_KEY=model-secret", "PATH=/normal/bin", "HOME=/normal/home", "CLIPSE_ISSUE_TEXT=task",
			}
		}),
	)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := ws.EnsuredIssues(); len(got) != 0 {
		t.Fatalf("local workspacer called in Daytona mode: %v", got)
	}
	if len(manager.ensures) != 1 {
		t.Fatalf("Ensure calls = %d, want 1", len(manager.ensures))
	}
	request := manager.ensures[0]
	if request.Provider != "daytona" || request.Role != "coder" || request.IssueID != "issue-1" || request.RunID != "run-1" {
		t.Errorf("Ensure request identity = %+v", request)
	}
	if request.RepoURL != "https://github.com/xlyk/clipse.git" || request.RepoSlug != "xlyk/clipse" || request.BaseBranch != "main" || request.Branch != "issue-1-branch" {
		t.Errorf("Ensure request repo metadata = %+v", request)
	}
	if request.AutoStopMinutes != 60 || request.ReviewerAutoDeleteMinutes != 30 || request.Snapshot != "snapshot-1" || request.Target != "us" {
		t.Errorf("Ensure request lifecycle config = %+v", request)
	}

	specs := spawner.Specs()
	if len(specs) != 1 {
		t.Fatalf("spawn specs = %d, want 1", len(specs))
	}
	spec := specs[0]
	if spec.Workspace != "/home/daytona/workspace/clipse" || spec.Backend != "daytona" || spec.SandboxID != "sandbox-1" {
		t.Errorf("Daytona worker spec = %+v", spec)
	}
	if spec.RepoURL != "https://github.com/xlyk/clipse.git" || spec.RepoSlug != "xlyk/clipse" || spec.Branch != "issue-1-branch" {
		t.Errorf("Daytona worker repo metadata = %+v", spec)
	}
	wantEnv := []string{
		"ANTHROPIC_API_KEY=model-secret", "PATH=/host/bin", "HOME=/host/home", "CLIPSE_ISSUE_TEXT=task",
		"DAYTONA_API_KEY=daytona-secret", "DAYTONA_API_URL=https://daytona.example", "DAYTONA_TARGET=us",
	}
	if !slices.Equal(spec.Env, wantEnv) {
		t.Errorf("Daytona Env = %v, want %v", spec.Env, wantEnv)
	}
	if _, ok := envValue(spec.Env, "LINEAR_API_KEY"); ok {
		t.Errorf("Daytona Env leaked LINEAR_API_KEY: %v", spec.Env)
	}

	recorded, err := s.AgentWorkspacesByIssue(context.Background(), "issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded workspaces = %+v", recorded)
	}
	if got := recorded[0]; got.OwnerKey != manager.workspace.OwnerKey || got.RunID != "run-1" || got.Role != "coder" || got.State != store.WorkspaceActive || got.LastAction != "ensure" {
		t.Errorf("recorded workspace = %+v", got)
	}
}

func TestSpawnAttempt_DaytonaDefaultEnvDropsHostGitHubTokens(t *testing.T) {
	t.Setenv("PATH", "/host/bin")
	t.Setenv("HOME", "/host/home")
	t.Setenv("GH_TOKEN", "gh-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("DAYTONA_API_KEY", "daytona-secret")

	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	spawner := newFakeSpawner()
	manager := &fakeBackendManager{workspace: backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:issue-1", ExternalID: "sandbox-1",
		WorkspacePath: "/home/daytona/workspace/clipse", State: "active",
	}}
	cfg := testConfig()
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	cfg.AgentBackend = config.AgentBackend{Type: "daytona", Daytona: config.DaytonaBackend{AutoStopMinutes: 60, ReviewerAutoDeleteMinutes: 60}}
	cfg.EnvAllowlist = []string{"PATH", "HOME", "GH_TOKEN", "GITHUB_TOKEN"}
	d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()), dispatcher.WithBackendManager(manager),
	)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	env := spawner.Specs()[0].Env
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if _, ok := envValue(env, key); ok {
			t.Errorf("Daytona worker Env leaked %s: %v", key, env)
		}
	}
	for _, key := range []string{"PATH", "HOME", "DAYTONA_API_KEY", "CLIPSE_ISSUE_TEXT"} {
		if _, ok := envValue(env, key); !ok {
			t.Errorf("Daytona worker Env missing %s: %v", key, env)
		}
	}
}

func TestReviewerWorkspaceCleanupQueuedForEveryWorkerExit(t *testing.T) {
	bk := contract.BlockKindNeedsInput
	tests := []struct {
		name       string
		result     spawn.Result
		wantStatus string
	}{
		{
			name: "pass",
			result: spawn.Result{Worker: contract.WorkerResult{
				Outcome: contract.WorkerResultOutcomeDone, Summary: "LGTM",
			}},
			wantStatus: string(contract.ColumnMerging),
		},
		{
			name: "changes requested",
			result: spawn.Result{Worker: contract.WorkerResult{
				Outcome: contract.WorkerResultOutcomeChangesRequested, Summary: "fix it",
			}},
			wantStatus: string(contract.ColumnRework),
		},
		{
			name: "blocked",
			result: spawn.Result{Worker: contract.WorkerResult{
				Outcome: contract.WorkerResultOutcomeBlocked, BlockKind: &bk, Summary: "needs input",
			}},
			wantStatus: string(contract.ColumnBlocked),
		},
		{
			name:       "malformed result",
			result:     spawn.Result{Err: fmt.Errorf("%w: invalid JSON", spawn.ErrMalformedResult)},
			wantStatus: string(contract.ColumnBlocked),
		},
		{
			name:       "worker crash",
			result:     spawn.Result{Err: fmt.Errorf("%w: exit code 1", spawn.ErrWorkerExit)},
			wantStatus: string(contract.ColumnBlocked),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			seedColumnIssue(t, s, "issue-1", string(contract.ColumnReview), 1, 100)
			coder := store.AgentWorkspace{
				OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", RunID: "run-coder",
				Provider: "daytona", Role: "coder", ExternalID: "sandbox-coder", WorkspacePath: "/workspace",
				State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
			}
			if err := s.UpsertAgentWorkspace(ctx, coder); err != nil {
				t.Fatal(err)
			}

			spawner := newFakeSpawner()
			spawner.Results["issue-1"] = tt.result
			manager := &fakeBackendManager{workspace: backend.Workspace{
				Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:reviewer:issue-1:run-1",
				ExternalID: "sandbox-reviewer", WorkspacePath: "/home/daytona/workspace/clipse", State: "active",
			}}
			cfg := testConfig()
			cfg.AgentBackend.Type = "daytona"
			cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
			cfg.Caps = config.Caps{Global: 1, PerLane: config.PerLaneCaps{Reviewer: 1}}
			d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
				dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
				dispatcher.WithBackendManager(manager),
			)

			if err := d.Tick(ctx); err != nil {
				t.Fatalf("claim reviewer: %v", err)
			}
			tickUntilCond(t, ctx, d, "reviewer result and cleanup queue", func() bool {
				issue := getIssue(t, s, "issue-1")
				workspaces, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
				if err != nil || len(workspaces) != 2 {
					return false
				}
				return issue.BoardStatus == tt.wantStatus && workspaces[1].State == store.WorkspaceCleanupPending
			})

			workspaces, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
			if err != nil {
				t.Fatal(err)
			}
			if workspaces[0].OwnerKey != coder.OwnerKey || workspaces[0].State != store.WorkspaceActive {
				t.Errorf("persistent coder workspace changed: %+v", workspaces[0])
			}
			if workspaces[1].RunID != "run-1" || workspaces[1].Role != "reviewer" || workspaces[1].State != store.WorkspaceCleanupPending {
				t.Errorf("reviewer workspace not queued precisely: %+v", workspaces[1])
			}
		})
	}
}

func TestReviewerWorkspaceCleanupMarkFailureRetainsWorkerResult(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnReview), 1, 100)
	spawner := newFakeSpawner()
	spawner.Results["issue-1"] = spawn.Result{Worker: contract.WorkerResult{
		Outcome: contract.WorkerResultOutcomeDone, Summary: "LGTM",
	}}
	manager := &fakeBackendManager{workspace: backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:reviewer:issue-1:run-1",
		ExternalID: "sandbox-reviewer", WorkspacePath: "/home/daytona/workspace/clipse", State: "active",
	}}
	cfg := testConfig()
	cfg.AgentBackend.Type = "daytona"
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	cfg.Caps = config.Caps{Global: 1, PerLane: config.PerLaneCaps{Reviewer: 1}}
	d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithBackendManager(manager),
	)

	if err := d.Tick(ctx); err != nil {
		t.Fatalf("claim reviewer: %v", err)
	}
	workspaces, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
	if err != nil || len(workspaces) != 1 {
		t.Fatalf("reviewer workspace before failure = %+v, err %v", workspaces, err)
	}
	reviewerWorkspace := workspaces[0]
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM agent_workspaces WHERE owner_key = ?`, reviewerWorkspace.OwnerKey); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var markErr error
	for time.Now().Before(deadline) {
		markErr = d.Tick(ctx)
		if markErr != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if markErr == nil || !strings.Contains(markErr.Error(), "reviewer workspace cleanup") {
		t.Fatalf("cleanup mark failure = %v, want surfaced reconciliation error", markErr)
	}
	if got := getIssue(t, s, "issue-1"); got.BoardStatus != string(contract.ColumnReview) {
		t.Fatalf("worker result applied despite cleanup mark failure: status %q", got.BoardStatus)
	}

	if err := s.UpsertAgentWorkspace(ctx, reviewerWorkspace); err != nil {
		t.Fatal(err)
	}
	tickUntilCond(t, ctx, d, "retained reviewer result", func() bool {
		workspaces, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
		return err == nil && len(workspaces) == 1 &&
			workspaces[0].State == store.WorkspaceCleanupPending &&
			getIssue(t, s, "issue-1").BoardStatus == string(contract.ColumnMerging)
	})
}

func TestSpawnAttempt_DaytonaRejectsUnsafeRemoteBeforeLifecycleOrArgv(t *testing.T) {
	const secret = "ghp_secret"
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	spawner := newFakeSpawner()
	manager := &fakeBackendManager{workspace: backend.Workspace{
		Provider: "daytona", OwnerKey: "owner", ExternalID: "sandbox-1",
		WorkspacePath: "/home/daytona/workspace/clipse", State: "active",
	}}
	cfg := testConfig()
	cfg.AgentBackend = config.AgentBackend{Type: "daytona", Daytona: config.DaytonaBackend{AutoStopMinutes: 60, ReviewerAutoDeleteMinutes: 60}}
	cfg.Repo.Remote = "git@github.com:x/y.git?token=" + secret
	d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()), dispatcher.WithBackendManager(manager),
	)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(manager.ensures) != 0 {
		t.Errorf("backend Ensure reached with unsafe remote: %+v", manager.ensures)
	}
	if spawner.SpawnCount() != 0 {
		t.Errorf("SpawnCount = %d, want 0 so no worker argv can carry unsafe remote", spawner.SpawnCount())
	}
	snapshot, err := s.ReadSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	issue := snapshot.Issues[0]
	if issue.BoardStatus != "blocked" {
		t.Errorf("BoardStatus = %q, want blocked", issue.BoardStatus)
	}
	if issue.LatestRun == nil || !issue.LatestRun.Error.Valid {
		t.Fatal("run error missing")
	}
	if strings.Contains(issue.LatestRun.Error.String, secret) || strings.Contains(issue.LatestRun.Error.String, cfg.Repo.Remote) {
		t.Errorf("stored error leaked unsafe remote: %q", issue.LatestRun.Error.String)
	}
}

func TestSpawnAttempt_LocalPreservesEnvironmentAndArgDefaults(t *testing.T) {
	t.Setenv("DAYTONA_API_KEY", "daytona-secret")
	t.Setenv("DAYTONA_API_URL", "https://daytona.example")
	t.Setenv("DAYTONA_TARGET", "us")

	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	spawner := newFakeSpawner()
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.AgentBackend.Type = "local"
	wantEnv := []string{"ANTHROPIC_API_KEY=model-secret", "PATH=/normal/bin", "HOME=/normal/home", "CLIPSE_ISSUE_TEXT=task"}
	d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, ws,
		dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithEnvFor(func(store.Issue) []string { return append([]string(nil), wantEnv...) }),
	)
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	spec := spawner.Specs()[0]
	if !slices.Equal(spec.Env, wantEnv) {
		t.Errorf("local Env = %v, want byte-for-byte %v", spec.Env, wantEnv)
	}
	if spec.Backend != "" || spec.SandboxID != "" || spec.RepoURL != "" || spec.RepoSlug != "" || spec.Branch != "" {
		t.Errorf("local spec gained Daytona metadata: %+v", spec)
	}
}

func TestSpawnAttempt_DaytonaProvisionErrorsRespectKind(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus string
		wantRetry  int
	}{
		{"transient retries", &backend.ActionError{Kind: backend.ErrorKindTransient, Op: "ensure", Msg: "provider unavailable"}, "ready", 1},
		{"needs input parks", &backend.ActionError{Kind: backend.ErrorKindNeedsInput, Op: "github_auth", Msg: "authenticate GitHub"}, "blocked", 0},
		{"capability parks", &backend.ActionError{Kind: backend.ErrorKindCapability, Op: "ensure", Msg: "unsupported SDK"}, "blocked", 0},
		{"untyped parks", errors.New("unexpected lifecycle failure"), "blocked", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
			spawner := newFakeSpawner()
			cfg := testConfig()
			cfg.AgentBackend.Type = "daytona"
			cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
			cfg.RecoverCap = 2
			cfg.RecoverBackoffS = 1
			manager := &fakeBackendManager{err: tt.err}
			d := dispatcher.New(cfg, s, &linear.MockClient{}, spawner, newStubWorkspacer(t.TempDir()),
				dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()), dispatcher.WithBackendManager(manager),
			)
			if err := d.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
			issue, err := s.GetIssue(context.Background(), "issue-1")
			if err != nil {
				t.Fatal(err)
			}
			if issue.BoardStatus != tt.wantStatus || issue.RecoverAttempts != tt.wantRetry {
				t.Errorf("status/recover attempts = %s/%d, want %s/%d", issue.BoardStatus, issue.RecoverAttempts, tt.wantStatus, tt.wantRetry)
			}
			if spawner.SpawnCount() != 0 {
				t.Errorf("SpawnCount = %d, want 0", spawner.SpawnCount())
			}
		})
	}
}

var _ backend.Manager = (*fakeBackendManager)(nil)
