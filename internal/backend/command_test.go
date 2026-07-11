package backend

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type recordingRunner struct {
	stdout []byte
	err    error
	argv   []string
	env    []string
}

func (r *recordingRunner) Run(_ context.Context, argv, env []string) ([]byte, error) {
	r.argv = append([]string(nil), argv...)
	r.env = append([]string(nil), env...)
	return r.stdout, r.err
}

func TestCommandManagerEnsure_UsesSanitizedDaytonaEnv(t *testing.T) {
	runner := &recordingRunner{stdout: []byte(`{"ok":true,"provider":"daytona","owner_key":"k","external_id":"sb-1","workspace_path":"/home/daytona/workspace/clipse","state":"active"}`)}
	m := NewCommandManager([]string{"uv", "run", "clipse-worker"}, runner.Run, []string{
		"PATH=/bin", "HOME=/tmp/home", "DAYTONA_API_KEY=secret", "DAYTONA_API_URL=https://daytona.example", "DAYTONA_TARGET=us",
		"LINEAR_API_KEY=never", "GH_TOKEN=never-either",
	})

	workspace, err := m.Ensure(context.Background(), EnsureRequest{
		Provider:                  "daytona",
		Role:                      "coder",
		IssueID:                   "i",
		RunID:                     "r",
		RepoURL:                   "https://github.com/x/y.git",
		RepoSlug:                  "x/y",
		BaseBranch:                "main",
		Branch:                    "i-branch",
		AutoStopMinutes:           60,
		ReviewerAutoDeleteMinutes: 30,
		Snapshot:                  "snapshot-1",
		Target:                    "us",
	})
	if err != nil {
		t.Fatal(err)
	}
	if workspace.ExternalID != "sb-1" || workspace.WorkspacePath != "/home/daytona/workspace/clipse" {
		t.Fatalf("workspace = %+v", workspace)
	}
	if slices.Contains(runner.env, "LINEAR_API_KEY=never") || slices.Contains(runner.env, "GH_TOKEN=never-either") {
		t.Fatalf("kernel or unrelated secret forwarded: %v", runner.env)
	}
	for _, want := range []string{
		"PATH=/bin", "HOME=/tmp/home", "DAYTONA_API_KEY=secret", "DAYTONA_API_URL=https://daytona.example", "DAYTONA_TARGET=us",
	} {
		if !slices.Contains(runner.env, want) {
			t.Errorf("env %q missing from %v", want, runner.env)
		}
	}
	for _, want := range []string{
		"uv", "run", "clipse-worker", "--backend-action=ensure", "--backend-provider=daytona", "--backend-role=coder",
		"--issue=i", "--run=r", "--repo-url=https://github.com/x/y.git", "--repo-slug=x/y", "--base-branch=main",
		"--branch=i-branch", "--auto-stop-minutes=60", "--reviewer-auto-delete-minutes=30", "--snapshot=snapshot-1", "--target=us",
	} {
		if !slices.Contains(runner.argv, want) {
			t.Errorf("argv %q missing from %#v", want, runner.argv)
		}
	}
}

func TestCommandManagerDeleteAndList_UseLifecycleFlags(t *testing.T) {
	t.Run("delete", func(t *testing.T) {
		runner := &recordingRunner{stdout: []byte(`{"ok":true,"provider":"daytona","owner_key":"k","external_id":"sb-1","workspace_path":"/home/daytona/workspace/clipse","state":"deleted"}`)}
		m := NewCommandManager([]string{"worker"}, runner.Run, nil)
		err := m.Delete(context.Background(), Workspace{
			Provider: "daytona", OwnerKey: "k", ExternalID: "sb-1", WorkspacePath: "/home/daytona/workspace/clipse",
			State: "active", Role: "reviewer", IssueID: "i", RunID: "r", RepoSlug: "x/y",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--backend-action=delete", "--sandbox-id=sb-1", "--backend-role=reviewer", "--issue=i", "--run=r", "--repo-slug=x/y"} {
			if !slices.Contains(runner.argv, want) {
				t.Errorf("argv %q missing from %#v", want, runner.argv)
			}
		}
	})

	t.Run("list", func(t *testing.T) {
		runner := &recordingRunner{stdout: []byte(`{"ok":true,"provider":"daytona","workspaces":[{"owner_key":"k","external_id":"sb-1","workspace_path":"/home/daytona/workspace/clipse","state":"stopped"}]}`)}
		m := NewCommandManager([]string{"worker"}, runner.Run, nil)
		workspaces, err := m.List(context.Background(), ListRequest{Provider: "daytona", RepoSlug: "x/y", Target: "us"})
		if err != nil {
			t.Fatal(err)
		}
		if len(workspaces) != 1 || workspaces[0].ExternalID != "sb-1" || workspaces[0].RepoSlug != "x/y" {
			t.Fatalf("workspaces = %+v", workspaces)
		}
		for _, want := range []string{"--backend-action=list", "--backend-provider=daytona", "--repo-slug=x/y", "--target=us"} {
			if !slices.Contains(runner.argv, want) {
				t.Errorf("argv %q missing from %#v", want, runner.argv)
			}
		}
	})
}

func TestCommandManagerErrors_AreTypedAndSanitized(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
		runErr error
		kind   ErrorKind
	}{
		{
			name:   "typed needs input",
			stdout: `{"ok":false,"provider":"daytona","error_kind":"needs_input","error_operation":"github_auth","error":"authenticate GitHub; provider token was secret-token"}`,
			kind:   ErrorKindNeedsInput,
		},
		{
			name:   "malformed JSON",
			stdout: `{"ok":true,"provider":"daytona","DAYTONA_API_KEY":"secret-token"`,
			kind:   ErrorKindCapability,
		},
		{
			name:   "nonzero process exit",
			runErr: errors.New("exit status 1: stderr leaked secret-token"),
			kind:   ErrorKindTransient,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingRunner{stdout: []byte(tt.stdout), err: tt.runErr}
			m := NewCommandManager([]string{"worker"}, runner.Run, []string{"DAYTONA_API_KEY=secret-token"})
			_, err := m.Ensure(context.Background(), EnsureRequest{Provider: "daytona"})
			if err == nil {
				t.Fatal("Ensure error = nil")
			}
			var actionErr *ActionError
			if !errors.As(err, &actionErr) {
				t.Fatalf("error type = %T, want *ActionError: %v", err, err)
			}
			if actionErr.Kind != tt.kind {
				t.Errorf("Kind = %q, want %q", actionErr.Kind, tt.kind)
			}
			if strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "stderr leaked") || strings.Contains(err.Error(), "DAYTONA_API_KEY") {
				t.Errorf("error leaked sensitive/raw provider data: %v", err)
			}
		})
	}
}

func TestCommandManagerRejectsMultipleJSONObjects(t *testing.T) {
	runner := &recordingRunner{stdout: []byte("{\"ok\":true}\n{\"ok\":true}\n")}
	m := NewCommandManager([]string{"worker"}, runner.Run, nil)
	_, err := m.List(context.Background(), ListRequest{Provider: "daytona", RepoSlug: "x/y"})
	if err == nil {
		t.Fatal("List error = nil")
	}
	var actionErr *ActionError
	if !errors.As(err, &actionErr) || actionErr.Kind != ErrorKindCapability {
		t.Fatalf("error = %T %v, want capability ActionError", err, err)
	}
}

func TestCanonicalGitHubRemote(t *testing.T) {
	tests := []struct {
		name          string
		remote        string
		wantURL       string
		wantSlug      string
		wantErr       bool
		forbiddenText []string
	}{
		{
			name:     "clean HTTPS",
			remote:   "https://github.com/x/y.git",
			wantURL:  "https://github.com/x/y.git",
			wantSlug: "x/y",
		},
		{
			name:     "GitHub SCP SSH",
			remote:   "git@github.com:x/y.git",
			wantURL:  "https://github.com/x/y.git",
			wantSlug: "x/y",
		},
		{
			name:          "credential-bearing HTTPS",
			remote:        "https://user:token-secret@github.com/x/y.git",
			wantErr:       true,
			forbiddenText: []string{"user", "token-secret", "https://"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotSlug, err := CanonicalGitHubRemote(tt.remote)
			if tt.wantErr {
				if err == nil {
					t.Fatal("CanonicalGitHubRemote error = nil")
				}
				for _, forbidden := range tt.forbiddenText {
					if strings.Contains(err.Error(), forbidden) {
						t.Errorf("error %q leaked rejected remote content %q", err, forbidden)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if gotURL != tt.wantURL || gotSlug != tt.wantSlug {
				t.Errorf("CanonicalGitHubRemote() = %q, %q; want %q, %q", gotURL, gotSlug, tt.wantURL, tt.wantSlug)
			}
		})
	}
}
