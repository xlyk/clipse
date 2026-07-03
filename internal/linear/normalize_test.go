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

	issues, err := linear.NormalizeCandidateIssues(data)
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
	_, err := linear.NormalizeCandidateIssues([]byte("not json"))
	if err == nil {
		t.Fatal("NormalizeCandidateIssues: expected error for malformed JSON, got nil")
	}
}
