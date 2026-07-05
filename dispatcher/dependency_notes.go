package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/store"
)

// dependencyNotesCap bounds the assembled CLIPSE_DEPENDENCY_NOTES document so a
// long comment history can't blow up the worker's prompt. Oldest blocker
// comments are evicted first, then whole blockers, keeping the issue's own
// comments (the freshest rework/continuation context) last to go.
const dependencyNotesCap = 16_000

// depSection is one rendered block of the dependency-notes document: a heading
// naming the source issue and its role, plus that issue's comments oldest
// first. own marks the issue's own section, which is evicted last under the
// size cap.
type depSection struct {
	label    string // e.g. "REF-5 (blocker)" or "REF-9 (this issue)"
	comments []linear.Comment
	own      bool
}

// dependencyNotes renders the Linear comment history a coder needs at claim
// time: its blockers' comments (the decisions the ticket template tells it to
// read) plus its own (rework/continuation context). Best-effort: any fetch
// error degrades to "" -- a spawn must never fail because Linear was slow.
// Capped at dependencyNotesCap chars, oldest-first eviction, issue-own comments
// evicted last. Comment bodies are untrusted input and are never logged.
func (d *Dispatcher) dependencyNotes(ctx context.Context, issue store.Issue) string {
	var blockerIDs []string
	if issue.Deps != "" {
		if err := json.Unmarshal([]byte(issue.Deps), &blockerIDs); err != nil {
			d.logger.Warn("dependency notes: unmarshaling deps failed", "issue_id", issue.ID, "error", err)
			return ""
		}
	}

	sections := make([]depSection, 0, len(blockerIDs)+1)
	for _, blockerID := range blockerIDs {
		comments, err := d.linear.IssueComments(ctx, blockerID)
		if err != nil {
			d.logger.Warn("dependency notes: fetching blocker comments failed", "issue_id", issue.ID, "blocker_id", blockerID, "error", err)
			return ""
		}
		sections = append(sections, depSection{
			label:    fmt.Sprintf("%s (blocker)", d.blockerLabel(ctx, blockerID)),
			comments: sortedByCreatedAt(comments),
		})
	}

	ownComments, err := d.linear.IssueComments(ctx, issue.ID)
	if err != nil {
		d.logger.Warn("dependency notes: fetching issue comments failed", "issue_id", issue.ID, "error", err)
		return ""
	}
	sections = append(sections, depSection{
		label:    fmt.Sprintf("%s (this issue)", issue.Identifier),
		comments: sortedByCreatedAt(ownComments),
		own:      true,
	})

	return capDependencyNotes(sections)
}

// blockerLabel resolves a blocker's human identifier (e.g. "REF-5") for its
// section heading, falling back to the raw id when the blocker isn't cached
// locally (a dependency that never entered this board).
func (d *Dispatcher) blockerLabel(ctx context.Context, blockerID string) string {
	if b, err := d.store.GetIssue(ctx, blockerID); err == nil && b.Identifier != "" {
		return b.Identifier
	}
	return blockerID
}

// sortedByCreatedAt returns comments ordered oldest first. Linear's createdAt
// is an ISO-8601 string, which sorts lexicographically the same as
// chronologically, so a plain string compare is enough (and it keeps eviction
// oldest-first). It copies rather than sorting in place so a caller's slice
// (e.g. a MockClient's scripted table) is never mutated.
func sortedByCreatedAt(comments []linear.Comment) []linear.Comment {
	out := make([]linear.Comment, len(comments))
	copy(out, comments)
	// Insertion sort: comment lists are short (<=50) and this avoids pulling in
	// sort just for a stable oldest-first order.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt < out[j-1].CreatedAt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// capDependencyNotes renders sections and, while the document exceeds the cap,
// evicts the oldest blocker comment (dropping a blocker's heading once its last
// comment goes); only once no blocker comments remain does it start evicting
// the issue's own oldest comments. Eviction stops while one comment still
// remains rather than draining to nothing, so a lone comment that alone
// exceeds the cap is truncated to its head (on a rune boundary) instead of
// dropped entirely -- the coder still gets the start of it.
func capDependencyNotes(sections []depSection) string {
	doc := renderDependencyNotes(sections)
	for len(doc) > dependencyNotesCap && totalComments(sections) > 1 {
		if !evictOldestComment(sections) {
			break
		}
		doc = renderDependencyNotes(sections)
	}
	if len(doc) > dependencyNotesCap {
		doc = truncateToRuneBoundary(doc, dependencyNotesCap)
	}
	return doc
}

// totalComments counts the comments still held across all sections, so
// capDependencyNotes can stop evicting before it drains the last one.
func totalComments(sections []depSection) int {
	n := 0
	for i := range sections {
		n += len(sections[i].comments)
	}
	return n
}

// truncateToRuneBoundary returns at most maxBytes bytes of s, backing off any
// trailing byte that would split a multibyte UTF-8 rune, so the result is
// always valid UTF-8 and never longer than maxBytes.
func truncateToRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	trunc := s[:maxBytes]
	for len(trunc) > 0 {
		if r, size := utf8.DecodeLastRuneInString(trunc); r == utf8.RuneError && size <= 1 {
			trunc = trunc[:len(trunc)-1]
			continue
		}
		break
	}
	return trunc
}

// evictOldestComment removes one comment to shrink the document: the oldest
// blocker comment across all blocker sections first, and only the issue's own
// oldest comment once no blocker comments are left. Returns false when nothing
// remains to evict.
func evictOldestComment(sections []depSection) bool {
	best := -1
	for i := range sections {
		if sections[i].own || len(sections[i].comments) == 0 {
			continue
		}
		if best == -1 || sections[i].comments[0].CreatedAt < sections[best].comments[0].CreatedAt {
			best = i
		}
	}
	if best == -1 {
		for i := range sections {
			if sections[i].own && len(sections[i].comments) > 0 {
				best = i
				break
			}
		}
	}
	if best == -1 {
		return false
	}
	sections[best].comments = sections[best].comments[1:]
	return true
}

// renderDependencyNotes turns sections into the markdown document injected as
// CLIPSE_DEPENDENCY_NOTES. Empty sections (a blocker whose comments were all
// evicted, or an issue with no comments) are omitted entirely, so a fully
// evicted blocker contributes nothing -- not even a bare heading.
func renderDependencyNotes(sections []depSection) string {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		if len(s.comments) == 0 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "## %s comments\n", s.label)
		for _, c := range s.comments {
			b.WriteString("\n")
			b.WriteString(strings.TrimSpace(c.Body))
			b.WriteString("\n")
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	return strings.Join(parts, "\n\n")
}
