package dispatcher

import (
	"fmt"
	"strings"

	"github.com/xlyk/clipse/internal/contract"
)

// Linear renders GitHub-flavored markdown in issue comments. The dispatcher is
// the only automated commenter, and it comments on exactly the transitions a
// human needs to notice: a block (any kind) and a rework. These builders give
// those comments a consistent shape — an emoji + heading line naming the state,
// then the detail (a raw error/log dump fenced in a code block, a short human
// summary as prose) — so a Linear card reads like a review timeline instead of
// the raw single-line log dump it used to be.

// humanizeBlockKind turns a contract block_kind enum into a title-cased phrase
// for a comment heading. Every real value is mapped; an unknown one falls
// through verbatim so a future enum addition still renders something sensible.
func humanizeBlockKind(kind string) string {
	switch kind {
	case string(contract.BlockKindNeedsInput):
		return "Needs input"
	case string(contract.BlockKindTransient):
		return "Transient error"
	case string(contract.BlockKindCapability):
		return "Capability limit"
	default:
		return kind
	}
}

// looksLikeError reports whether detail is a raw error/log dump — multi-line,
// or carrying tell-tale command/stderr fragments — that reads better inside a
// fenced code block than as an inline prose paragraph.
func looksLikeError(detail string) bool {
	if strings.Contains(detail, "\n") {
		return true
	}
	for _, marker := range []string{"command failed", "stderr:", "error:", "exit ", "traceback"} {
		if strings.Contains(strings.ToLower(detail), marker) {
			return true
		}
	}
	return false
}

// detailBlock renders detail either as a fenced code block (raw errors/logs) or
// as a trimmed prose paragraph, or "" when there is nothing to show.
func detailBlock(detail string) string {
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return ""
	}
	if looksLikeError(detail) {
		return fmt.Sprintf("```\n%s\n```", detail)
	}
	return detail
}

// blockedComment renders a "blocked" Linear comment. kind is the worker's
// block_kind ("" for a run-level infra failure — crash, timeout, turn cap,
// spawn failure, orphan — which carries no kind, in which case the heading
// omits the "— kind" suffix). detail is the summary/reason, fenced when it
// looks like a raw error.
func blockedComment(kind, detail string) string {
	heading := "### 🚫 Blocked"
	if kind != "" {
		heading += " — " + humanizeBlockKind(kind)
	}
	if body := detailBlock(detail); body != "" {
		return heading + "\n\n" + body
	}
	return heading
}

// changesRequestedComment renders the Git-operator lane's stale-base-conflict
// comment: the only changes_requested route that posts a dispatcher-authored
// comment (the Reviewer lane posts its own inline PR review comments instead).
// summary already names the conflicting files (staleBaseConflictSummary).
func changesRequestedComment(summary string) string {
	heading := "### 🔧 Changes requested"
	body := detailBlock(summary)
	if body == "" {
		body = "The base branch moved on and this PR no longer merges cleanly. Rebase onto the latest base and push again."
	}
	return heading + "\n\n" + body
}

// retryComment renders auto-unblock layer 1's re-queue comment: an "auto-retry
// N/M" heading naming where the issue is in its transient-failure budget, then
// the failure reason (fenced when it looks like a raw error). It tells a human
// watching the board that the issue is recovering on its own, not stuck.
func retryComment(attempt, cap int, reason string) string {
	heading := fmt.Sprintf("### 🔄 Auto-retry %d/%d — transient failure", attempt, cap)
	if body := detailBlock(reason); body != "" {
		return heading + "\n\n" + body
	}
	return heading
}

// reworkCapComment renders the rework-cap block: the cap, the PR under review,
// and the last review summary that tipped it over, as a scannable bullet list
// so a human triaging the Blocked column sees why without opening the store.
func reworkCapComment(cap int, result contract.WorkerResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### ⛔ Blocked — rework cap reached (%d)\n\n", cap)
	b.WriteString("This issue cycled back to rework too many times without converging, so it's parked here for a human to take a look.\n")
	if result.PrUrl != nil && *result.PrUrl != "" {
		fmt.Fprintf(&b, "\n- **PR:** %s", *result.PrUrl)
	}
	if strings.TrimSpace(result.Summary) != "" {
		fmt.Fprintf(&b, "\n- **Last review:** %s", strings.TrimSpace(result.Summary))
	}
	return b.String()
}
