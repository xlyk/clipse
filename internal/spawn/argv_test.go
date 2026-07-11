package spawn

import (
	"slices"
	"testing"
)

// TestWorkerArgs covers the per-invocation flag list LocalSpawner.Spawn
// appends after its configured command prefix: the five fields every worker
// invocation carries, plus --checkpoint-db/--max-tokens ONLY when spec
// carries them (WorkerSpec.CheckpointDB non-empty / MaxTokens > 0). Kept as
// a pure helper so this conditional-append logic is unit-testable without
// spawning a real process — in particular, without ever having to teach
// testworker's strict flag.Parse about flags only the real clipse-worker
// understands (see internal/spawn/local_test.go's
// TestLocalSpawner_MultiElementCommand for the exec-plumbing side of this).
func TestWorkerArgs(t *testing.T) {
	tests := []struct {
		name string
		spec WorkerSpec
		want []string
	}{
		{
			name: "fixed five only, checkpoint-db and max-tokens both unset",
			spec: WorkerSpec{
				Issue:     "CLP-1",
				Lane:      "coder",
				RunID:     "run-1",
				ThreadID:  "thread-1",
				Workspace: "/ws",
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=coder",
				"--run=run-1",
				"--thread=thread-1",
				"--workspace=/ws",
			},
		},
		{
			name: "checkpoint-db appended when set",
			spec: WorkerSpec{
				Issue:        "CLP-1",
				Lane:         "coder",
				RunID:        "run-1",
				ThreadID:     "thread-1",
				Workspace:    "/ws",
				CheckpointDB: "/ckpt/CLP-1.db",
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=coder",
				"--run=run-1",
				"--thread=thread-1",
				"--workspace=/ws",
				"--checkpoint-db=/ckpt/CLP-1.db",
			},
		},
		{
			name: "max-tokens appended when positive",
			spec: WorkerSpec{
				Issue:     "CLP-1",
				Lane:      "coder",
				RunID:     "run-1",
				ThreadID:  "thread-1",
				Workspace: "/ws",
				MaxTokens: 50000,
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=coder",
				"--run=run-1",
				"--thread=thread-1",
				"--workspace=/ws",
				"--max-tokens=50000",
			},
		},
		{
			name: "max-tokens omitted when zero",
			spec: WorkerSpec{
				Issue:     "CLP-1",
				Workspace: "/ws",
				MaxTokens: 0,
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=",
				"--run=",
				"--thread=",
				"--workspace=/ws",
			},
		},
		{
			name: "checkpoint-db omitted when empty",
			spec: WorkerSpec{
				Issue:        "CLP-1",
				Workspace:    "/ws",
				CheckpointDB: "",
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=",
				"--run=",
				"--thread=",
				"--workspace=/ws",
			},
		},
		{
			name: "both checkpoint-db and max-tokens appended, in order after the fixed five",
			spec: WorkerSpec{
				Issue:        "CLP-1",
				Lane:         "coder",
				RunID:        "run-1",
				ThreadID:     "thread-1",
				Workspace:    "/ws",
				CheckpointDB: "/ckpt/CLP-1.db",
				MaxTokens:    50000,
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=coder",
				"--run=run-1",
				"--thread=thread-1",
				"--workspace=/ws",
				"--checkpoint-db=/ckpt/CLP-1.db",
				"--max-tokens=50000",
			},
		},
		{
			name: "model only",
			spec: WorkerSpec{
				Lane:  "coder",
				Model: "openai_codex:gpt-5.5",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				"--model=openai_codex:gpt-5.5",
			},
		},
		{
			name: "docs-model only",
			spec: WorkerSpec{
				Lane:      "coder",
				DocsModel: "anthropic:claude-sonnet-4-6",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				"--docs-model=anthropic:claude-sonnet-4-6",
			},
		},
		{
			name: "both empty omits both flags",
			spec: WorkerSpec{
				Lane: "reviewer",
			},
			want: []string{
				"--issue=",
				"--lane=reviewer",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
		{
			name: "model-params only",
			spec: WorkerSpec{
				Lane:        "coder",
				ModelParams: `{"reasoning_effort":"high"}`,
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				`--model-params={"reasoning_effort":"high"}`,
			},
		},
		{
			name: "docs-model-params only",
			spec: WorkerSpec{
				Lane:            "coder",
				DocsModelParams: `{"reasoning_effort":"high"}`,
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				`--docs-model-params={"reasoning_effort":"high"}`,
			},
		},
		{
			name: "model-params and docs-model-params both empty omits both flags",
			spec: WorkerSpec{
				Lane: "reviewer",
			},
			want: []string{
				"--issue=",
				"--lane=reviewer",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
		{
			name: "base-branch appended when set",
			spec: WorkerSpec{
				Lane:       "coder",
				BaseBranch: "main",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				"--base-branch=main",
			},
		},
		{
			name: "base-branch omitted when empty",
			spec: WorkerSpec{
				Lane: "coder",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
		{
			name: "shell-allow-list appended when set",
			spec: WorkerSpec{
				Lane:           "coder",
				ShellAllowList: `["git","gh"]`,
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				`--shell-allow-list=["git","gh"]`,
			},
		},
		{
			name: "docs-shell-allow-list appended when set",
			spec: WorkerSpec{
				Lane:               "coder",
				DocsShellAllowList: `["cat","ls"]`,
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				`--docs-shell-allow-list=["cat","ls"]`,
			},
		},
		{
			name: "shell-allow-list and docs-shell-allow-list both empty omits both flags",
			spec: WorkerSpec{
				Lane: "reviewer",
			},
			want: []string{
				"--issue=",
				"--lane=reviewer",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
		{
			name: "transcript appended when set",
			spec: WorkerSpec{
				Lane:           "coder",
				TranscriptPath: "/board/logs/CLP-1.transcript.jsonl",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
				"--transcript=/board/logs/CLP-1.transcript.jsonl",
			},
		},
		{
			name: "transcript omitted when empty",
			spec: WorkerSpec{
				Lane: "coder",
			},
			want: []string{
				"--issue=",
				"--lane=coder",
				"--run=",
				"--thread=",
				"--workspace=",
			},
		},
		{
			name: "daytona backend metadata appended when set",
			spec: WorkerSpec{
				Issue:     "CLP-1",
				Lane:      "coder",
				RunID:     "run-1",
				ThreadID:  "thread-1",
				Workspace: "/home/daytona/workspace/clipse",
				Backend:   "daytona",
				SandboxID: "sandbox-1",
				RepoURL:   "https://github.com/xlyk/clipse.git",
				RepoSlug:  "xlyk/clipse",
				Branch:    "CLP-1-branch",
			},
			want: []string{
				"--issue=CLP-1",
				"--lane=coder",
				"--run=run-1",
				"--thread=thread-1",
				"--workspace=/home/daytona/workspace/clipse",
				"--backend=daytona",
				"--sandbox-id=sandbox-1",
				"--repo-url=https://github.com/xlyk/clipse.git",
				"--repo-slug=xlyk/clipse",
				"--branch=CLP-1-branch",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workerArgs(tt.spec)
			if !slices.Equal(got, tt.want) {
				t.Errorf("workerArgs(%+v) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}
