package tui

import "testing"

// TestKindLabel asserts raw event kinds map to short, fixed-width-friendly
// labels so the activity feed never truncates a kind mid-word (the old feed
// showed "rework_cap_ex…" / "request_chang…").
func TestKindLabel(t *testing.T) {
	tests := []struct {
		kind string
		want string
	}{
		{"rework_cap_exceeded", "rework cap"},
		{"request_changes", "changes req"},
		{"open_review", "review"},
		{"claimed", "claimed"},
		{"auto_merged", "merged"},
		{"done", "complete"},
		{"complete", "complete"},
		{"promoted", "promoted"},
		{"blocked", "blocked"},
		{"stale_claim_released", "claimed"}, // "claim" matches before "stale"/"release"
		{"queued", "queued"},                // unknown, short: passed through
	}
	for _, tt := range tests {
		if got := kindLabel(tt.kind); got != tt.want {
			t.Errorf("kindLabel(%q) = %q, want %q", tt.kind, got, tt.want)
		}
		if got := kindLabel(tt.kind); len([]rune(got)) > 11 {
			t.Errorf("kindLabel(%q) = %q is wider than the 11-col kind column", tt.kind, got)
		}
	}
}

// TestCleanActivityDetail asserts the feed detail is de-noised: a "claimed"
// event (whose detail is the redundant "claimed by run <uuid>") collapses to a
// short run id, and multi-line details flatten to one line.
func TestCleanActivityDetail(t *testing.T) {
	tests := []struct {
		name, kind, detail, want string
	}{
		{"claimed strips prefix + shortens uuid", "claimed", "claimed by run 8494b1cc1690b9e368059c9db9d6717c", "run 8494b1cc"},
		{"claimed short id kept", "claimed", "claimed by run abc123", "run abc123"},
		{"multiline flattened", "open_review", "line one\nline two", "line one line two"},
		{"whitespace collapsed", "request_changes", "  a   b\t c ", "a b c"},
	}
	for _, tt := range tests {
		if got := cleanActivityDetail(tt.kind, tt.detail); got != tt.want {
			t.Errorf("%s: cleanActivityDetail(%q, %q) = %q, want %q", tt.name, tt.kind, tt.detail, got, tt.want)
		}
	}
}
