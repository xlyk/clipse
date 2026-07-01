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

	t.Run("deps parsed from blocks/blocked-by relations", func(t *testing.T) {
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
