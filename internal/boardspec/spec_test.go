package boardspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseResolvesBodyFile(t *testing.T) {
	spec, err := Parse("testdata/simple.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Team != "CLI" {
		t.Errorf("Team = %q, want CLI", spec.Team)
	}
	if len(spec.Issues) != 2 {
		t.Fatalf("Issues = %d, want 2", len(spec.Issues))
	}
	if got := spec.Issues[0].Body; got != "inline core body\n" {
		t.Errorf("inline body = %q", got)
	}
	// cfg-1 uses body_file; Parse must have loaded and cleared it.
	if got := spec.Issues[1].Body; got != "config from file\n" {
		t.Errorf("file body = %q", got)
	}
	if spec.Issues[1].BodyFile != "" {
		t.Errorf("BodyFile not cleared: %q", spec.Issues[1].BodyFile)
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "board.yaml")
	if err := os.WriteFile(path, []byte(`team: CLI
issues:
  - ref: a
    title: A
    body: body
    depps: [b]
`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := Parse(path)
	if err == nil || !strings.Contains(err.Error(), "field depps not found") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestParseRejectsBodyAndBodyFileTogether(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "board.yaml")
	if err := os.WriteFile(filepath.Join(dir, "body.md"), []byte("from file"), 0o600); err != nil {
		t.Fatalf("WriteFile body: %v", err)
	}
	if err := os.WriteFile(path, []byte(`team: CLI
issues:
  - ref: a
    title: A
    body: inline
    body_file: body.md
`), 0o600); err != nil {
		t.Fatalf("WriteFile spec: %v", err)
	}
	_, err := Parse(path)
	if err == nil || !strings.Contains(err.Error(), "exactly one of body or body_file") {
		t.Fatalf("want body-source error, got %v", err)
	}
}
