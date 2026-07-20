package linear_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
	"github.com/xlyk/clipse/internal/linear"
)

func TestNormalizeCandidateIssues_FromFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "candidate_issues.json"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	issues, err := linear.NormalizeCandidateIssues(data, "agent:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}

	if len(issues) != 4 {
		t.Fatalf("len(issues) = %d, want 4", len(issues))
	}

	byIdentifier := make(map[string]linear.Issue, len(issues))
	for _, iss := range issues {
		byIdentifier[iss.Identifier] = iss
	}

	t.Run("lane label stripped to bare lane", func(t *testing.T) {
		got := byIdentifier["CLP-12"]
		if got.Lane != string(contract.LaneCoder) {
			t.Errorf("Lane = %q, want %q", got.Lane, contract.LaneCoder)
		}
	})

	t.Run("status mapped to column enum case-insensitively", func(t *testing.T) {
		tests := []struct {
			identifier string
			want       contract.Column
		}{
			{"CLP-12", contract.ColumnTodo},
			{"CLP-13", contract.ColumnReview},
			{"CLP-14", contract.ColumnReady},
			{"CLP-15", contract.ColumnMerging},
		}
		for _, tt := range tests {
			got := byIdentifier[tt.identifier]
			if got.Status != string(tt.want) {
				t.Errorf("%s: Status = %q, want %q", tt.identifier, got.Status, tt.want)
			}
		}
	})

	// A dependency of X is an issue that BLOCKS X. In Linear's canonical data
	// model a blocking relationship is one record on the *blocker's* source
	// side (type "blocks", relatedIssue = the blocked issue); from the blocked
	// issue's perspective it appears in inverseRelations (type "blocks", issue
	// = the blocker). So Deps are read from inverseRelations, NOT the
	// source-side `relations` the query used to fetch — that inverted the
	// direction (a dependent looked dependency-free and promoted immediately,
	// while its blocker waited on it), which the live smoke exposed.
	t.Run("deps parsed from inverse blocks relations (the blockers)", func(t *testing.T) {
		got := byIdentifier["CLP-12"]
		wantDeps := []string{"22222222-2222-2222-2222-222222222222"}
		if len(got.Deps) != len(wantDeps) || got.Deps[0] != wantDeps[0] {
			t.Errorf("CLP-12 Deps = %v, want %v", got.Deps, wantDeps)
		}

		got14 := byIdentifier["CLP-14"]
		wantDeps14 := []string{"11111111-1111-1111-1111-111111111111"}
		if len(got14.Deps) != len(wantDeps14) || got14.Deps[0] != wantDeps14[0] {
			t.Errorf("CLP-14 Deps = %v, want %v", got14.Deps, wantDeps14)
		}

		got13 := byIdentifier["CLP-13"]
		if len(got13.Deps) != 0 {
			t.Errorf("CLP-13 Deps = %v, want empty", got13.Deps)
		}

		// CLP-15's only inverse relation is "related", not "blocks" — a
		// related/duplicate/similar link is not a dependency, so it must not
		// gate promotion.
		got15 := byIdentifier["CLP-15"]
		if len(got15.Deps) != 0 {
			t.Errorf("CLP-15 Deps = %v, want empty (a 'related' link is not a blocker)", got15.Deps)
		}
	})

	t.Run("priority, branch name, id, identifier populated", func(t *testing.T) {
		got := byIdentifier["CLP-12"]
		if got.ID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("ID = %q, want %q", got.ID, "11111111-1111-1111-1111-111111111111")
		}
		if got.Priority != 2 {
			t.Errorf("Priority = %d, want 2", got.Priority)
		}
		if got.BranchName != "clp-12-add-thing" {
			t.Errorf("BranchName = %q, want %q", got.BranchName, "clp-12-add-thing")
		}
		if got.UpdatedAt != 1782907200 {
			t.Errorf("UpdatedAt = %d, want unix %d", got.UpdatedAt, 1782907200)
		}
	})

	t.Run("title and description parsed", func(t *testing.T) {
		got := byIdentifier["CLP-12"]
		if got.Title != "Add the thing" {
			t.Errorf("Title = %q, want %q", got.Title, "Add the thing")
		}
		if got.Description != "Implement the thing that does the stuff." {
			t.Errorf("Description = %q, want %q", got.Description, "Implement the thing that does the stuff.")
		}

		// CLP-13's fixture description is an explicit empty string -- must
		// parse as "", not be mistaken for a missing/omitted field.
		got13 := byIdentifier["CLP-13"]
		if got13.Title != "Review the thing" {
			t.Errorf("CLP-13 Title = %q, want %q", got13.Title, "Review the thing")
		}
		if got13.Description != "" {
			t.Errorf("CLP-13 Description = %q, want empty", got13.Description)
		}
	})

	t.Run("no agent label leaves lane empty, not an error", func(t *testing.T) {
		got := byIdentifier["CLP-14"]
		if got.Lane != "" {
			t.Errorf("Lane = %q, want empty (no agent:<lane> label present)", got.Lane)
		}
	})
}

func TestNormalizeCandidateIssues_MalformedJSON(t *testing.T) {
	_, err := linear.NormalizeCandidateIssues([]byte("not json"), "agent:")
	if err == nil {
		t.Fatal("NormalizeCandidateIssues: expected error for malformed JSON, got nil")
	}
}

func TestNormalizeCandidateIssues_CancelledStateTypeOverridesName(t *testing.T) {
	// The state's NAME is deliberately something a name-based lookup would
	// never recognize ("Won't Fix") -- proving the mapping is driven by the
	// fixed, unrenameable state TYPE, not the team-configurable display name.
	const raw = `{
		"data": {
			"issues": {
				"nodes": [
					{
						"id": "cancelled-1",
						"identifier": "CLP-16",
						"title": "Abandoned thing",
						"description": "",
						"priority": 3,
						"branchName": "clp-16-abandoned",
						"updatedAt": "2026-07-01T16:00:00.000Z",
						"state": { "name": "Won't Fix", "type": "canceled" },
						"labels": { "nodes": [] },
						"inverseRelations": { "nodes": [] }
					}
				]
			}
		}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "agent:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	if issues[0].Status != "cancelled" {
		t.Errorf("Status = %q, want %q (type-driven, not name-driven)", issues[0].Status, "cancelled")
	}
}

func TestNormalizeCandidateIssues_TerminalBlockersDroppedFromDeps(t *testing.T) {
	// A blocker that is already completed or canceled in Linear is satisfied
	// at ingest and must not appear in Deps at all. Blockers outside the
	// candidate set (unlabeled teammates' tickets, shipped work) are never
	// ingested, so a dep pointing at one can never become terminal on the
	// board -- promote would hold the child in todo forever (2026-07-08
	// Spacelift relaunch: SPA-872 stuck behind Done-but-unlabeled SPA-868).
	// A live blocker (unstarted/started) stays in Deps; a blocker with NO
	// state in the payload stays too (unknown = conservatively live).
	const raw = `{
		"data": {
			"issues": {
				"nodes": [
					{
						"id": "child-1",
						"identifier": "SPA-872",
						"title": "Child",
						"description": "",
						"priority": 3,
						"branchName": "spa-872",
						"updatedAt": "2026-07-01T16:00:00.000Z",
						"state": { "name": "Todo", "type": "unstarted" },
						"labels": { "nodes": [{ "name": "agent:coder" }] },
						"inverseRelations": { "nodes": [
							{ "type": "blocks", "issue": { "id": "done-blocker", "state": { "type": "completed" } } },
							{ "type": "blocks", "issue": { "id": "cancelled-blocker", "state": { "type": "canceled" } } },
							{ "type": "blocks", "issue": { "id": "live-blocker", "state": { "type": "started" } } },
							{ "type": "blocks", "issue": { "id": "stateless-blocker" } }
						] }
					}
				]
			}
		}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "agent:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	wantDeps := []string{"live-blocker", "stateless-blocker"}
	got := issues[0].Deps
	if len(got) != len(wantDeps) || got[0] != wantDeps[0] || got[1] != wantDeps[1] {
		t.Errorf("Deps = %v, want %v (terminal blockers satisfied at ingest)", got, wantDeps)
	}
}

func TestNormalizeCandidateIssues_CompletedStateTypeOverridesName(t *testing.T) {
	// A completed-type state with a team-specific NAME ("Ready for Release",
	// as on the Spacelift team) must read as done, not fall back to todo --
	// the todo fallback would let a mislabeled, already-shipped ticket be
	// claimed and re-run. Same type-over-name rationale as the cancelled
	// case above; "completed" is equally fixed and unrenameable.
	const raw = `{
		"data": {
			"issues": {
				"nodes": [
					{
						"id": "shipped-1",
						"identifier": "SPA-999",
						"title": "Shipped thing",
						"description": "",
						"priority": 3,
						"branchName": "spa-999-shipped",
						"updatedAt": "2026-07-01T16:00:00.000Z",
						"state": { "name": "Ready for Release", "type": "completed" },
						"labels": { "nodes": [] },
						"inverseRelations": { "nodes": [] }
					}
				]
			}
		}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "agent:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	if issues[0].Status != "done" {
		t.Errorf("Status = %q, want %q (type-driven, not name-driven)", issues[0].Status, "done")
	}
}

func TestNormalizeCandidateIssues_CustomLabelPrefix(t *testing.T) {
	const raw = `{"data":{"issues":{"nodes":[{"id":"id-1","identifier":"CLI-1","title":"t","description":"","priority":0,"branchName":"b","updatedAt":"2026-07-01T00:00:00.000Z","state":{"name":"Todo"},"labels":{"nodes":[{"name":"clipse:reviewer"}]},"inverseRelations":{"nodes":[]}}]}}}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("len(issues) = %d, want 1", len(issues))
	}
	if issues[0].Lane != string(contract.LaneReviewer) {
		t.Errorf("Lane = %q, want %q (a configured prefix must be honored, not the hardcoded \"agent:\")", issues[0].Lane, contract.LaneReviewer)
	}

	// The SAME label spelling under the default "agent:" prefix must NOT
	// parse when a different prefix is configured -- proves this isn't
	// falling back to a hardcoded default under the hood.
	const rawAgent = `{"data":{"issues":{"nodes":[{"id":"id-2","identifier":"CLI-2","title":"t","description":"","priority":0,"branchName":"b","updatedAt":"2026-07-01T00:00:00.000Z","state":{"name":"Todo"},"labels":{"nodes":[{"name":"agent:coder"}]},"inverseRelations":{"nodes":[]}}]}}}`
	issuesAgent, err := linear.NormalizeCandidateIssues([]byte(rawAgent), "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: unexpected error: %v", err)
	}
	if issuesAgent[0].Lane != "" {
		t.Errorf("Lane = %q, want empty (an \"agent:\" label must not match a configured \"clipse:\" prefix)", issuesAgent[0].Lane)
	}
}

func TestNormalizeCandidateIssues_StateLabelMode(t *testing.T) {
	const raw = `{
		"data": {"issues": {"nodes": [
			{
				"id": "rework", "identifier": "SPA-1", "title": "rework", "description": "",
				"priority": 0, "branchName": "spa-1", "updatedAt": "2026-07-01T00:00:00.000Z",
				"state": {"name": "In Progress", "type": "started"},
				"labels": {"nodes": [{"name": "agent:coder"}, {"name": "clipse:rework"}]},
				"inverseRelations": {"nodes": []}
			},
			{
				"id": "unseeded", "identifier": "SPA-2", "title": "unseeded", "description": "",
				"priority": 0, "branchName": "spa-2", "updatedAt": "2026-07-01T00:00:00.000Z",
				"state": {"name": "In Progress", "type": "started"},
				"labels": {"nodes": [{"name": "agent:coder"}]},
				"inverseRelations": {"nodes": []}
			},
			{
				"id": "terminal", "identifier": "SPA-3", "title": "terminal", "description": "",
				"priority": 0, "branchName": "spa-3", "updatedAt": "2026-07-01T00:00:00.000Z",
				"state": {"name": "Ready for Release", "type": "completed"},
				"labels": {"nodes": [{"name": "agent:coder"}, {"name": "clipse:ready"}]},
				"inverseRelations": {"nodes": []}
			},
			{
				"id": "conflict", "identifier": "SPA-4", "title": "conflict", "description": "",
				"priority": 0, "branchName": "spa-4", "updatedAt": "2026-07-01T00:00:00.000Z",
				"state": {"name": "Todo", "type": "unstarted"},
				"labels": {"nodes": [{"name": "agent:coder"}, {"name": "clipse:ready"}, {"name": "clipse:review"}]},
				"inverseRelations": {"nodes": []}
			}
		]}}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "agent:", "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: %v", err)
	}
	byID := make(map[string]linear.Issue, len(issues))
	for _, issue := range issues {
		byID[issue.ID] = issue
	}
	if got := byID["rework"].Status; got != string(contract.ColumnRework) {
		t.Errorf("rework Status = %q, want rework (label overrides workflow state)", got)
	}
	if got := byID["unseeded"].Status; got != string(contract.ColumnTodo) {
		t.Errorf("unseeded Status = %q, want todo (label mode ignores active workflow names)", got)
	}
	if got := byID["terminal"].Status; got != string(contract.ColumnDone) {
		t.Errorf("terminal Status = %q, want done (completed workflow type is a safety override)", got)
	}
	if got := byID["conflict"].Status; got != string(contract.ColumnBlocked) {
		t.Errorf("conflict Status = %q, want blocked (ambiguous state labels fail safe)", got)
	}
}

func TestNormalizeCandidateIssues_StateLabelDoneSatisfiesDependency(t *testing.T) {
	const raw = `{
		"data": {"issues": {"nodes": [{
			"id": "child", "identifier": "SPA-2", "title": "child", "description": "",
			"priority": 0, "branchName": "spa-2", "updatedAt": "2026-07-01T00:00:00.000Z",
			"state": {"name": "Todo", "type": "unstarted"},
			"labels": {"nodes": [{"name": "agent:coder"}, {"name": "clipse:todo"}]},
			"inverseRelations": {"nodes": [{
				"type": "blocks",
				"issue": {
					"id": "blocker", "state": {"type": "unstarted"},
					"labels": {"nodes": [{"name": "clipse:done"}]}
				}
			}]}
		}]}}
	}`

	issues, err := linear.NormalizeCandidateIssues([]byte(raw), "agent:", "clipse:")
	if err != nil {
		t.Fatalf("NormalizeCandidateIssues: %v", err)
	}
	if got := issues[0].Deps; len(got) != 0 {
		t.Errorf("Deps = %v, want empty (clipse:done blocker is terminal)", got)
	}
}
