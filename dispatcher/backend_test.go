package dispatcher_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/backend"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/linear"
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
	if request.RepoURL != "git@github.com:xlyk/clipse.git" || request.RepoSlug != "xlyk/clipse" || request.BaseBranch != "main" || request.Branch != "issue-1-branch" {
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
	if spec.RepoURL != cfg.Repo.Remote || spec.RepoSlug != "xlyk/clipse" || spec.Branch != "issue-1-branch" {
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
