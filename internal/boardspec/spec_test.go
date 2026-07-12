package boardspec

import "testing"

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
