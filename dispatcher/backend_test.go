package dispatcher_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/gitops"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

type fakeBackendManager struct {
	mu        sync.Mutex
	workspace backend.Workspace
	err       error
	deleteErr error
	deleteFn  func(backend.Workspace) error
	listErr   error
	ensures   []backend.EnsureRequest
	deletes   []backend.Workspace
	listed    []backend.Workspace
	listCalls int
}

func (f *fakeBackendManager) Ensure(_ context.Context, request backend.EnsureRequest) (backend.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensures = append(f.ensures, request)
	return f.workspace, f.err
}

func (f *fakeBackendManager) Delete(_ context.Context, workspace backend.Workspace) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, workspace)
	deleteFn := f.deleteFn
	deleteErr := f.deleteErr
	f.mu.Unlock()
	if deleteFn != nil {
		return deleteFn(workspace)
	}
	return deleteErr
}

func (f *fakeBackendManager) List(context.Context, backend.ListRequest) ([]backend.Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return append([]backend.Workspace(nil), f.listed...), f.listErr
}

func (f *fakeBackendManager) deletedWorkspaces() []backend.Workspace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]backend.Workspace(nil), f.deletes...)
}

func (f *fakeBackendManager) listCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
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
			}, deleteErr: &backend.ActionError{Kind: backend.ErrorKindTransient, Op: "delete", Msg: "provider unavailable"}}
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
	}, deleteErr: &backend.ActionError{Kind: backend.ErrorKindTransient, Op: "delete", Msg: "provider unavailable"}}
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
	workspace := mustAgentWorkspace(t, s, "issue-1", "local:coder:issue-1")
	if workspace.Provider != "local" || workspace.Role != "coder" || workspace.State != store.WorkspaceActive || workspace.WorkspacePath != spec.Workspace {
		t.Fatalf("local lifecycle row = %+v", workspace)
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

func TestWorkspaceCleanupFailureRemainsPending(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", RunID: "run-1",
		Provider: "daytona", Role: "coder", ExternalID: "sb-1", WorkspacePath: "/workspace",
		State: store.WorkspaceCleanupPending, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	manager := &fakeBackendManager{deleteErr: &backend.ActionError{
		Kind: backend.ErrorKindTransient, Op: "delete", Msg: "provider unavailable",
	}}
	d := newDaytonaBackendDispatcher(t, s, manager)

	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	workspace := mustAgentWorkspace(t, s, "issue-1", "daytona:xlyk/clipse:coder:issue-1")
	if workspace.State != store.WorkspaceCleanupPending {
		t.Fatalf("state = %q, want cleanup_pending", workspace.State)
	}
	if workspace.LastError != "provider unavailable" {
		t.Fatalf("last error = %q, want sanitized provider message", workspace.LastError)
	}
	if got := manager.deletedWorkspaces(); len(got) != 1 || got[0].ExternalID != "sb-1" {
		t.Fatalf("Delete calls = %+v", got)
	}
}

func TestWorkspaceCleanupRetriesUntilSuccess(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", Provider: "daytona", Role: "coder",
		ExternalID: "sb-1", WorkspacePath: "/workspace", State: store.WorkspaceCleanupPending,
		LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	manager := &fakeBackendManager{deleteErr: errors.New("raw provider detail must not persist")}
	d := newDaytonaBackendDispatcher(t, s, manager)

	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", "daytona:xlyk/clipse:coder:issue-1")
	if workspace.LastError != "workspace cleanup failed" {
		t.Fatalf("untyped cleanup error = %q, want generic sanitized message", workspace.LastError)
	}
	manager.deleteErr = nil
	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	workspace = mustAgentWorkspace(t, s, "issue-1", "daytona:xlyk/clipse:coder:issue-1")
	if workspace.State != store.WorkspaceDeleted || workspace.LastError != "" {
		t.Fatalf("workspace after retry = %+v", workspace)
	}
	if got := len(manager.deletedWorkspaces()); got != 2 {
		t.Fatalf("Delete calls = %d, want 2", got)
	}
}

func TestTerminalDoneQueuesCoderWorkspaceAtomically(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnMerging), 1, 100)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", Provider: "daytona", Role: "coder",
		ExternalID: "sb-1", WorkspacePath: "/workspace", State: store.WorkspaceActive,
		LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	manager := &fakeBackendManager{}
	cfg := testConfig()
	cfg.AgentBackend.Type = "daytona"
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	d := dispatcher.New(cfg, s, &linear.MockClient{}, newFakeSpawner(), newStubWorkspacer(t.TempDir()),
		dispatcher.WithBackendManager(manager), dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsPreChecker(func(context.Context, gitops.Spec) (gitops.Result, bool, error) {
			return gitops.Result{Outcome: gitops.OutcomeMerged, PRNumber: 42}, true, nil
		}),
	)

	if err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if issue := getIssue(t, s, "issue-1"); issue.BoardStatus != string(contract.ColumnDone) {
		t.Fatalf("board status = %q, want done", issue.BoardStatus)
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", "daytona:xlyk/clipse:coder:issue-1")
	if workspace.State != store.WorkspaceCleanupPending || workspace.UpdatedAt != 1000 {
		t.Fatalf("terminal coder workspace = %+v", workspace)
	}
}

func TestReconcileAgentWorkspacesDeletesUnknownIssueOrphan(t *testing.T) {
	s := openTestStore(t)
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:unknown-issue", ExternalID: "sb-orphan",
		WorkspacePath: "/workspace", State: backend.WorkspaceActive,
	}
	manager := &fakeBackendManager{listed: []backend.Workspace{remote}}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool { return len(manager.deletedWorkspaces()) == 1 })

	if got := manager.deletedWorkspaces(); len(got) != 1 || got[0].ExternalID != "sb-orphan" {
		t.Fatalf("deleted workspaces = %+v", got)
	}
	workspace := mustAgentWorkspace(t, s, "unknown-issue", remote.OwnerKey)
	if workspace.State != store.WorkspaceDeleted {
		t.Fatalf("orphan state = %q, want deleted", workspace.State)
	}
}

func TestReconcileAgentWorkspacesDefersTerminalOpenRunCleanupUntilRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if err := s.UpsertIssue(ctx, store.Issue{
		ID: "issue-1", Identifier: "issue-1", LaneLabel: "coder", BoardStatus: "done", Deps: `[]`,
		BranchName: "issue-1-branch", CreatedAt: 10, UpdatedAt: 10, LastSeen: 10,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertRun(ctx, store.Run{RunID: "run-open", IssueID: "issue-1", Lane: "coder", Status: "running", StartedAt: 10, Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:issue-1", ExternalID: "sb-live",
		WorkspacePath: "/workspace", State: backend.WorkspaceError,
	}
	deletedWhileRunOpen := false
	manager := &fakeBackendManager{listed: []backend.Workspace{remote}}
	manager.deleteFn = func(backend.Workspace) error {
		runs, err := s.ListOpenRuns(ctx)
		if err != nil {
			return err
		}
		if len(runs) > 0 {
			deletedWhileRunOpen = true
		}
		return nil
	}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		rows, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
		return err == nil && len(rows) == 1 && rows[0].State == store.WorkspaceDeleted
	})

	if deletedWhileRunOpen {
		t.Fatal("workspace was deleted before orphan recovery closed its open run")
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", remote.OwnerKey)
	if workspace.State != store.WorkspaceDeleted {
		t.Fatalf("workspace state after recovery = %q, want deleted", workspace.State)
	}
	if got := manager.deletedWorkspaces(); len(got) != 1 || got[0].ExternalID != "sb-live" {
		t.Fatalf("Delete calls after orphan recovery = %+v", got)
	}
}

func TestReconcileAgentWorkspacesRestoresMissingCoderRow(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnBlocked), 1, 100)
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:issue-1", ExternalID: "sb-1",
		WorkspacePath: "/workspace", State: backend.WorkspaceActive,
	}
	manager := &fakeBackendManager{listed: []backend.Workspace{remote}}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		rows, err := s.AgentWorkspacesByIssue(context.Background(), "issue-1")
		return err == nil && len(rows) == 1
	})

	workspace := mustAgentWorkspace(t, s, "issue-1", remote.OwnerKey)
	if workspace.State != store.WorkspaceActive || workspace.LastAction != "reconcile" {
		t.Fatalf("restored workspace = %+v", workspace)
	}
	if got := manager.deletedWorkspaces(); len(got) != 0 {
		t.Fatalf("restored coder workspace was deleted: %+v", got)
	}
}

func TestReconcileAgentWorkspacesMarksMissingRemoteDeleted(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnBlocked), 1, 100)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "daytona:xlyk/clipse:coder:issue-1", IssueID: "issue-1", Provider: "daytona", Role: "coder",
		ExternalID: "sb-gone", WorkspacePath: "/workspace", State: store.WorkspaceActive,
		LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	manager := &fakeBackendManager{}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		return mustAgentWorkspace(t, s, "issue-1", "daytona:xlyk/clipse:coder:issue-1").State == store.WorkspaceDeleted
	})

	if got := manager.deletedWorkspaces(); len(got) != 0 {
		t.Fatalf("provider Delete called for already-missing remote: %+v", got)
	}
}

func TestReconcileAgentWorkspacesDoesNotRewriteLocalProviderRows(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnBlocked), 1, 100)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "local:coder:issue-1", IssueID: "issue-1", Provider: "local", Role: "coder",
		WorkspacePath: "/workspace", State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	manager := &fakeBackendManager{}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool { return lc.Count() > 0 })

	workspace := mustAgentWorkspace(t, s, "issue-1", "local:coder:issue-1")
	if workspace.State != store.WorkspaceActive || workspace.LastAction != "ensure" {
		t.Fatalf("Daytona reconciliation rewrote local provider row: %+v", workspace)
	}
}

func TestReconcileAgentWorkspacesQueuesReviewerOrphan(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnBlocked), 1, 100)
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:reviewer:issue-1:run-old", ExternalID: "sb-reviewer",
		WorkspacePath: "/workspace", State: backend.WorkspaceActive,
	}
	manager := &fakeBackendManager{
		listed:    []backend.Workspace{remote},
		deleteErr: &backend.ActionError{Kind: backend.ErrorKindTransient, Op: "delete", Msg: "provider unavailable"},
	}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		rows, err := s.AgentWorkspacesByIssue(context.Background(), "issue-1")
		return err == nil && len(rows) == 1 && rows[0].State == store.WorkspaceCleanupPending && rows[0].LastError != ""
	})

	workspace := mustAgentWorkspace(t, s, "issue-1", remote.OwnerKey)
	if workspace.State != store.WorkspaceCleanupPending || workspace.LastError != "provider unavailable" {
		t.Fatalf("reviewer orphan = %+v", workspace)
	}
	if got := manager.deletedWorkspaces(); len(got) != 1 || got[0].RunID != "run-old" {
		t.Fatalf("reviewer orphan Delete calls = %+v", got)
	}
}

func TestReconcileAgentWorkspacesDefersOpenReviewerCleanupUntilRecovery(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnReview), 1, 100)
	claim, err := s.ClaimColumn(ctx, string(contract.ColumnReview), string(contract.LaneReviewer), "run-review", 1000, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE runs SET attempt = 3 WHERE run_id = ?`, claim.Run.RunID); err != nil {
		t.Fatal(err)
	}
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:reviewer:issue-1:run-review", ExternalID: "sb-reviewer-live",
		WorkspacePath: "/workspace", State: backend.WorkspaceError,
	}
	deletedWhileRunOpen := false
	manager := &fakeBackendManager{listed: []backend.Workspace{remote}}
	manager.deleteFn = func(backend.Workspace) error {
		runs, err := s.ListOpenRuns(ctx)
		if err != nil {
			return err
		}
		deletedWhileRunOpen = len(runs) > 0
		return nil
	}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		rows, err := s.AgentWorkspacesByIssue(ctx, "issue-1")
		return err == nil && len(rows) == 1 && rows[0].State == store.WorkspaceDeleted
	})

	if deletedWhileRunOpen {
		t.Fatal("reviewer workspace was deleted before orphan recovery closed its run")
	}
	if got := manager.deletedWorkspaces(); len(got) != 1 || got[0].RunID != "run-review" {
		t.Fatalf("reviewer Delete calls after recovery = %+v", got)
	}
}

func TestReconcileAgentWorkspacesDuplicateCoderNeedsInputWithoutDeletion(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnBlocked), 1, 100)
	owner := "daytona:xlyk/clipse:coder:issue-1"
	manager := &fakeBackendManager{listed: []backend.Workspace{
		{Provider: "daytona", OwnerKey: owner, ExternalID: "sb-1", WorkspacePath: "/workspace", State: backend.WorkspaceActive},
		{Provider: "daytona", OwnerKey: owner, ExternalID: "sb-2", WorkspacePath: "/workspace", State: backend.WorkspaceActive},
	}}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
	runBackendUntil(t, d, lc, func() bool {
		events, err := s.ListEvents(context.Background())
		if err != nil {
			return false
		}
		for _, event := range events {
			if event.Kind == "workspace_reconcile_needs_input" {
				return true
			}
		}
		return false
	})

	if got := manager.deletedWorkspaces(); len(got) != 0 {
		t.Fatalf("duplicate coder workspaces were auto-deleted: %+v", got)
	}
	if rows, err := s.AgentWorkspacesByIssue(context.Background(), "issue-1"); err != nil || len(rows) != 0 {
		t.Fatalf("duplicate coder workspaces should not be collapsed: rows=%+v err=%v", rows, err)
	}
}

func TestReconcileAgentWorkspacesDuplicateCoderNeutralizesDurableCleanup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		boardStatus string
		state       store.WorkspaceState
	}{
		{name: "pending row", boardStatus: string(contract.ColumnBlocked), state: store.WorkspaceCleanupPending},
		{name: "terminal active row", boardStatus: string(contract.ColumnDone), state: store.WorkspaceActive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			seedColumnIssue(t, s, "issue-1", tc.boardStatus, 1, 100)
			owner := "daytona:xlyk/clipse:coder:issue-1"
			seedAgentWorkspace(t, s, store.AgentWorkspace{
				OwnerKey: owner, IssueID: "issue-1", Provider: "daytona", Role: "coder", ExternalID: "sb-1",
				WorkspacePath: "/workspace", State: tc.state, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
			})
			manager := &fakeBackendManager{listed: []backend.Workspace{
				{Provider: "daytona", OwnerKey: owner, ExternalID: "sb-1", WorkspacePath: "/workspace", State: backend.WorkspaceActive},
				{Provider: "daytona", OwnerKey: owner, ExternalID: "sb-2", WorkspacePath: "/workspace", State: backend.WorkspaceActive},
			}}
			d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(time.Hour))
			runBackendUntil(t, d, lc, func() bool {
				workspace := mustAgentWorkspace(t, s, "issue-1", owner)
				return workspace.State == store.WorkspaceError && workspace.LastAction == "reconcile_needs_input"
			})

			if got := manager.deletedWorkspaces(); len(got) != 0 {
				t.Fatalf("duplicate durable row was auto-deleted: %+v", got)
			}
			workspace := mustAgentWorkspace(t, s, "issue-1", owner)
			if !strings.Contains(workspace.LastError, "sb-1") || !strings.Contains(workspace.LastError, "sb-2") {
				t.Fatalf("duplicate diagnostic lacks sandbox ids: %+v", workspace)
			}
		})
	}
}

func TestReconcileAgentWorkspacesListsOncePerRun(t *testing.T) {
	s := openTestStore(t)
	manager := &fakeBackendManager{}
	d, lc := newDaytonaBackendRunDispatcher(t, s, manager, dispatcher.WithPollInterval(5*time.Millisecond))
	runBackendUntil(t, d, lc, func() bool { return manager.listCallCount() >= 1 })
	if got := manager.listCallCount(); got != 1 {
		t.Fatalf("List calls = %d, want exactly one startup reconciliation", got)
	}
}

func TestRun_GracefulShutdownLeavesLiveDaytonaWorkspace(t *testing.T) {
	s := openTestStore(t)
	seedReadyIssue(t, s, "issue-1", "coder", 1, 100)
	gate := &gatedSpawner{release: make(chan struct{})}
	remote := backend.Workspace{
		Provider: "daytona", OwnerKey: "daytona:xlyk/clipse:coder:issue-1", ExternalID: "sb-live",
		WorkspacePath: "/workspace", State: backend.WorkspaceActive,
	}
	manager := &fakeBackendManager{workspace: remote}
	cfg := testConfig()
	cfg.AgentBackend.Type = "daytona"
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	cfg.MaxRuntimeS = 3600
	d := dispatcher.New(cfg, s, &linear.MockClient{}, gate, newStubWorkspacer(t.TempDir()),
		dispatcher.WithBackendManager(manager), dispatcher.WithClock(fixedClock(1000)),
		dispatcher.WithRunIDGenerator(sequentialRunIDs()), dispatcher.WithPollInterval(10*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && gate.spawnedCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if gate.spawnedCount() == 0 {
		cancel()
		<-done
		t.Fatal("Daytona worker was not spawned")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := manager.deletedWorkspaces(); len(got) != 0 {
		t.Fatalf("graceful shutdown deleted live Daytona workspace: %+v", got)
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", remote.OwnerKey)
	if workspace.State != store.WorkspaceActive {
		t.Fatalf("live workspace state after shutdown = %q, want active", workspace.State)
	}
	if gate.handleCtxErr() != nil {
		t.Fatalf("live Daytona worker context was cancelled: %v", gate.handleCtxErr())
	}
	close(gate.release)
}

func TestLocalCancelledIssueRemovesWorktreeIdempotently(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnTodo), 1, 100)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "local:coder:issue-1", IssueID: "issue-1", Provider: "local", Role: "coder",
		WorkspacePath: "/workspace", State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	lc := &linear.MockClient{Issues: []linear.Issue{{
		ID: "issue-1", Identifier: "issue-1", Status: "cancelled", Lane: "coder", BranchName: "issue-1-branch", UpdatedAt: 200,
	}}}
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.AgentBackend.Type = "local"
	d := dispatcher.New(cfg, s, lc, newFakeSpawner(), ws, dispatcher.WithClock(fixedClock(1000)))

	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := ws.RemovedIssues(); !slices.Equal(got, []string{"issue-1", "issue-1"}) {
		t.Fatalf("Remove calls = %v, want idempotent cleanup on both cancelled observations", got)
	}
	if issue := getIssue(t, s, "issue-1"); issue.BoardStatus != "cancelled" {
		t.Fatalf("board status = %q, want cancelled", issue.BoardStatus)
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", "local:coder:issue-1")
	if workspace.State != store.WorkspaceDeleted || workspace.LastAction != "delete" {
		t.Fatalf("cancelled local lifecycle row = %+v", workspace)
	}
}

func TestTerminalDoneLocalCleanupMarksWorkspaceDeleted(t *testing.T) {
	s := openTestStore(t)
	seedColumnIssue(t, s, "issue-1", string(contract.ColumnMerging), 1, 100)
	seedAgentWorkspace(t, s, store.AgentWorkspace{
		OwnerKey: "local:coder:issue-1", IssueID: "issue-1", Provider: "local", Role: "coder",
		WorkspacePath: "/workspace", State: store.WorkspaceActive, LastAction: "ensure", CreatedAt: 10, UpdatedAt: 10,
	})
	ws := newStubWorkspacer(t.TempDir())
	cfg := testConfig()
	cfg.AgentBackend.Type = "local"
	d := dispatcher.New(cfg, s, &linear.MockClient{}, newFakeSpawner(), ws,
		dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
		dispatcher.WithGitOpsPreChecker(func(context.Context, gitops.Spec) (gitops.Result, bool, error) {
			return gitops.Result{Outcome: gitops.OutcomeMerged, PRNumber: 42}, true, nil
		}),
	)

	if err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace := mustAgentWorkspace(t, s, "issue-1", "local:coder:issue-1")
	if workspace.State != store.WorkspaceDeleted || workspace.LastAction != "delete" {
		t.Fatalf("done local lifecycle row = %+v", workspace)
	}
	if got := ws.RemovedIssues(); !slices.Equal(got, []string{"issue-1"}) {
		t.Fatalf("local done Remove calls = %v", got)
	}
}

func newDaytonaBackendDispatcher(t *testing.T, s *store.Store, manager *fakeBackendManager, opts ...dispatcher.Option) *dispatcher.Dispatcher {
	t.Helper()
	cfg := testConfig()
	cfg.AgentBackend.Type = "daytona"
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	base := []dispatcher.Option{
		dispatcher.WithBackendManager(manager), dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	}
	base = append(base, opts...)
	return dispatcher.New(cfg, s, &linear.MockClient{}, newFakeSpawner(), newStubWorkspacer(t.TempDir()), base...)
}

func newDaytonaBackendRunDispatcher(t *testing.T, s *store.Store, manager *fakeBackendManager, opts ...dispatcher.Option) (*dispatcher.Dispatcher, *countingLinearClient) {
	t.Helper()
	cfg := testConfig()
	cfg.AgentBackend.Type = "daytona"
	cfg.Repo.Remote = "https://github.com/xlyk/clipse.git"
	base := []dispatcher.Option{
		dispatcher.WithBackendManager(manager), dispatcher.WithClock(fixedClock(1000)), dispatcher.WithRunIDGenerator(sequentialRunIDs()),
	}
	base = append(base, opts...)
	lc := &countingLinearClient{}
	return dispatcher.New(cfg, s, lc, newFakeSpawner(), newStubWorkspacer(t.TempDir()), base...), lc
}

func runBackendUntil(t *testing.T, d *dispatcher.Dispatcher, lc *countingLinearClient, condition func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	ready := func() bool { return lc.Count() > 0 && condition() }
	for time.Now().Before(deadline) && !ready() {
		select {
		case err := <-done:
			t.Fatalf("Run returned before startup condition: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !ready() {
		cancel()
		<-done
		t.Fatal("startup reconciliation condition was not reached")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func seedAgentWorkspace(t *testing.T, s *store.Store, workspace store.AgentWorkspace) {
	t.Helper()
	if err := s.UpsertAgentWorkspace(context.Background(), workspace); err != nil {
		t.Fatalf("UpsertAgentWorkspace: %v", err)
	}
}

func mustAgentWorkspace(t *testing.T, s *store.Store, issueID, ownerKey string) store.AgentWorkspace {
	t.Helper()
	rows, err := s.AgentWorkspacesByIssue(context.Background(), issueID)
	if err != nil {
		t.Fatalf("AgentWorkspacesByIssue: %v", err)
	}
	for _, workspace := range rows {
		if workspace.OwnerKey == ownerKey {
			return workspace
		}
	}
	t.Fatalf("workspace %q not found in %+v", ownerKey, rows)
	return store.AgentWorkspace{}
}

var _ backend.Manager = (*fakeBackendManager)(nil)
