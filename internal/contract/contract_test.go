package contract

import (
	"encoding/json"
	"testing"
)

// validWorkerResultJSON is a hand-written, schema-valid WorkerResult payload
// (see schema/worker-result.schema.json). It exercises every required field
// plus the cross-file Lane enum ref into schema/board.schema.json.
const validWorkerResultJSON = `{
	"run_id": "run-1",
	"issue_id": "SPAC-123",
	"lane": "coder",
	"outcome": "done",
	"block_kind": null,
	"summary": "did the thing",
	"artifacts": ["path/to/file.go"],
	"thread_id": "thread-1",
	"turn_count": 3,
	"tokens": {"in": 100, "out": 200}
}`

func TestWorkerResult_UnmarshalJSON_RoundTripsKeyFields(t *testing.T) {
	var got WorkerResult
	if err := json.Unmarshal([]byte(validWorkerResultJSON), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if got.Outcome != WorkerResultOutcomeDone {
		t.Errorf("Outcome = %q, want %q", got.Outcome, WorkerResultOutcomeDone)
	}
	if got.Lane != LaneCoder {
		t.Errorf("Lane = %q, want %q", got.Lane, LaneCoder)
	}
	if got.Tokens.In != 100 {
		t.Errorf("Tokens.In = %d, want 100", got.Tokens.In)
	}
	if got.Tokens.Out != 200 {
		t.Errorf("Tokens.Out = %d, want 200", got.Tokens.Out)
	}
	if got.RunId != "run-1" {
		t.Errorf("RunId = %q, want %q", got.RunId, "run-1")
	}
	if got.IssueId != "SPAC-123" {
		t.Errorf("IssueId = %q, want %q", got.IssueId, "SPAC-123")
	}
	if got.ThreadId != "thread-1" {
		t.Errorf("ThreadId = %q, want %q", got.ThreadId, "thread-1")
	}
	if got.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", got.TurnCount)
	}
}

func TestWorkerResult_UnmarshalJSON_RejectsMissingRequiredField(t *testing.T) {
	missingLane := `{
		"run_id": "run-1",
		"issue_id": "SPAC-123",
		"outcome": "done",
		"summary": "did the thing",
		"artifacts": [],
		"thread_id": "thread-1",
		"turn_count": 0,
		"tokens": {"in": 0, "out": 0}
	}`

	var got WorkerResult
	if err := json.Unmarshal([]byte(missingLane), &got); err == nil {
		t.Fatal("Unmarshal: expected error for missing required field \"lane\", got nil")
	}
}
