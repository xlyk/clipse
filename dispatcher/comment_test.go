package dispatcher

import (
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/contract"
)

// A multi-line / error-shaped block summary must land inside a fenced code
// block, not be dumped inline as prose (the readability fix for the raw
// git-stderr wall the scribe push failure produced).
func TestBlockedComment_ErrorDetailGoesInCodeBlock(t *testing.T) {
	got := blockedComment(string(contract.BlockKindTransient),
		"clipse-worker internal error: command failed (exit 1): git push\nstderr: rejected non-fast-forward")

	if !strings.Contains(got, "### 🚫 Blocked — Transient error") {
		t.Errorf("missing humanized heading, got:\n%s", got)
	}
	if !strings.Contains(got, "```") {
		t.Errorf("multi-line error detail should be fenced in a code block, got:\n%s", got)
	}
}

// A short, human-readable summary reads better as a prose paragraph than
// wrapped in a code fence.
func TestBlockedComment_SimpleSummaryIsProse(t *testing.T) {
	got := blockedComment(string(contract.BlockKindNeedsInput), "need the staging API base url to continue")

	if strings.Contains(got, "```") {
		t.Errorf("a simple summary should not be fenced, got:\n%s", got)
	}
	if !strings.Contains(got, "Needs input") {
		t.Errorf("heading should humanize the block kind, got:\n%s", got)
	}
	if !strings.Contains(got, "need the staging API base url") {
		t.Errorf("summary text missing, got:\n%s", got)
	}
}

// A run-level failure carries no block_kind; the heading must omit the
// "— kind" suffix rather than print a bogus one.
func TestBlockedComment_NoKindOmitsSuffix(t *testing.T) {
	got := blockedComment("", "turn cap reached")

	if strings.Contains(got, "—") {
		t.Errorf("no block kind should omit the em-dash suffix, got:\n%s", got)
	}
	if !strings.HasPrefix(got, "### 🚫 Blocked") {
		t.Errorf("want a blocked heading, got:\n%s", got)
	}
	if !strings.Contains(got, "turn cap reached") {
		t.Errorf("reason text missing, got:\n%s", got)
	}
}

// The Git-operator stale-base-conflict comment must keep naming the
// conflicting files (folded into the summary) — a documented behavior other
// tests assert — while gaining a heading.
func TestChangesRequestedComment_NamesFilesFromSummary(t *testing.T) {
	got := changesRequestedComment("base branch moved on (conflicting files: foo.go, bar.go)")

	if !strings.Contains(got, "### 🔧 Changes requested") {
		t.Errorf("missing heading, got:\n%s", got)
	}
	if !strings.Contains(got, "foo.go") {
		t.Errorf("must keep conflicting file name, got:\n%s", got)
	}
}

// The rework-cap block must surface the cap, the PR link, and the last review
// summary as scannable markdown — the fields rework_test.go asserts on.
func TestReworkCapComment_HasCapPRAndReview(t *testing.T) {
	url := "https://github.com/x/y/pull/9"
	got := reworkCapComment(3, contract.WorkerResult{PrUrl: &url, Summary: "review 3: still not good enough"})

	for _, want := range []string{"rework cap", "3", url, "review 3"} {
		if !strings.Contains(got, want) {
			t.Errorf("comment missing %q, got:\n%s", want, got)
		}
	}
}
