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
