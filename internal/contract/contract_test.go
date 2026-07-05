package contract

import (
	"encoding/json"
	"testing"
)

// validWorkerResultJSON is a hand-written, schema-valid WorkerResult payload
// (see schema/worker-result.schema.json). It exercises every required field
// plus the cross-file Lane enum ref into schema/board.schema.json.
//
// block_kind is OMITTED here (not blocked), reflecting the "present iff
// outcome == blocked" invariant (see amendment X2).
const validWorkerResultJSON = `{
	"run_id": "run-1",
	"issue_id": "SPAC-123",
	"lane": "coder",
	"outcome": "done",
	"summary": "did the thing",
	"artifacts": ["path/to/file.go"],
	"thread_id": "thread-1",
	"turn_count": 3,
	"tokens": {"in": 100, "out": 200}
}`

// blockedWorkerResultJSON is the counterpart payload where outcome ==
// "blocked" and block_kind is present with a valid enum value.
const blockedWorkerResultJSON = `{
	"run_id": "run-2",
	"issue_id": "SPAC-124",
	"lane": "coder",
	"outcome": "blocked",
	"block_kind": "needs_input",
	"summary": "waiting on input",
	"artifacts": [],
	"thread_id": "thread-2",
	"turn_count": 1,
	"tokens": {"in": 10, "out": 20}
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

func TestWorkerResult_BlockKind_PresentIffBlocked(t *testing.T) {
	// Non-blocked result: block_kind omitted from the JSON entirely.
	var done WorkerResult
	if err := json.Unmarshal([]byte(validWorkerResultJSON), &done); err != nil {
		t.Fatalf("Unmarshal (done): %v", err)
	}
	if done.Outcome != WorkerResultOutcomeDone {
		t.Errorf("Outcome = %q, want %q", done.Outcome, WorkerResultOutcomeDone)
	}
	if done.BlockKind != nil {
		t.Errorf("BlockKind = %v, want nil for non-blocked result", done.BlockKind)
	}

	// Blocked result: block_kind present with a valid enum value.
	var blocked WorkerResult
	if err := json.Unmarshal([]byte(blockedWorkerResultJSON), &blocked); err != nil {
		t.Fatalf("Unmarshal (blocked): %v", err)
	}
	if blocked.Outcome != WorkerResultOutcomeBlocked {
		t.Errorf("Outcome = %q, want %q", blocked.Outcome, WorkerResultOutcomeBlocked)
	}
	if blocked.BlockKind == nil {
		t.Fatal("BlockKind = nil, want set for blocked result")
	}
	if *blocked.BlockKind != BlockKindNeedsInput {
		t.Errorf("BlockKind = %v, want %q", *blocked.BlockKind, BlockKindNeedsInput)
	}

	// Marshalling the non-blocked result must OMIT block_kind entirely
	// (not emit it as a null-valued key) — this is the wart X2 removes.
	out, err := json.Marshal(done)
	if err != nil {
		t.Fatalf("Marshal (done): %v", err)
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(out, &asMap); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	if _, present := asMap["block_kind"]; present {
		t.Errorf("marshalled done result has block_kind key, want omitted: %s", out)
	}

	// Marshalling the blocked result must include block_kind.
	outBlocked, err := json.Marshal(blocked)
	if err != nil {
		t.Fatalf("Marshal (blocked): %v", err)
	}
	var asMapBlocked map[string]interface{}
	if err := json.Unmarshal(outBlocked, &asMapBlocked); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	if v, present := asMapBlocked["block_kind"]; !present || v != "needs_input" {
		t.Errorf("marshalled blocked result block_kind = %v (present=%v), want %q", v, present, "needs_input")
	}
}

func TestWorkerResult_Handoff_OptionalRoundTrip(t *testing.T) {
	// A result carrying a handoff note round-trips the field.
	const withHandoff = `{
		"run_id": "run-1",
		"issue_id": "SPAC-1",
		"lane": "coder",
		"outcome": "needs_review",
		"summary": "did the thing",
		"artifacts": [],
		"handoff": "- chose drop semantics\n- added Widget.build",
		"thread_id": "thread-1",
		"turn_count": 1,
		"tokens": {"in": 1, "out": 2}
	}`
	var got WorkerResult
	if err := json.Unmarshal([]byte(withHandoff), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Handoff == nil {
		t.Fatal("Handoff = nil, want the handoff note")
	}
	if want := "- chose drop semantics\n- added Widget.build"; *got.Handoff != want {
		t.Errorf("Handoff = %q, want %q", *got.Handoff, want)
	}

	// A result without a handoff omits the key entirely when marshalled
	// (the PrUrl optional-pointer pattern — omitempty,omitzero).
	var noHandoff WorkerResult
	if err := json.Unmarshal([]byte(validWorkerResultJSON), &noHandoff); err != nil {
		t.Fatalf("Unmarshal (no handoff): %v", err)
	}
	if noHandoff.Handoff != nil {
		t.Errorf("Handoff = %v, want nil when absent", noHandoff.Handoff)
	}
	out, err := json.Marshal(noHandoff)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(out, &asMap); err != nil {
		t.Fatalf("Unmarshal into map: %v", err)
	}
	if _, present := asMap["handoff"]; present {
		t.Errorf("marshalled result without handoff has handoff key, want omitted: %s", out)
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
