package dispatcher

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xlyk/clipse/internal/linear"
)

// depComment is a terse constructor for the linear.Comment fixtures the
// eviction/cap tests below build in volume.
func depComment(body, createdAt string) linear.Comment {
	return linear.Comment{Body: body, CreatedAt: createdAt}
}

// TestSortedByCreatedAt_OldestFirstAndNoMutation covers the ordering the
// oldest-first eviction rule depends on, and that the input slice (often a
// MockClient's scripted table) is never mutated.
func TestSortedByCreatedAt_OldestFirstAndNoMutation(t *testing.T) {
	in := []linear.Comment{
		depComment("c", "2026-07-03T00:00:00.000Z"),
		depComment("a", "2026-07-01T00:00:00.000Z"),
		depComment("b", "2026-07-02T00:00:00.000Z"),
	}

	out := sortedByCreatedAt(in)

	if got := []string{out[0].Body, out[1].Body, out[2].Body}; got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("sortedByCreatedAt not oldest-first: %v", got)
	}
	if in[0].Body != "c" {
		t.Errorf("input slice mutated: in[0].Body = %q, want %q", in[0].Body, "c")
	}
}

// TestCapDependencyNotes_Eviction exercises the size cap directly (not through
// Tick): the happy-path injection test only ever renders two tiny comments, so
// this is the only coverage that forces real eviction.
func TestCapDependencyNotes_Eviction(t *testing.T) {
	filler := func(n int) string { return strings.Repeat("x", n) }

	cases := []struct {
		name        string
		sections    []depSection
		wantEmpty   bool
		wantPresent []string
		wantAbsent  []string
	}{
		{
			// Four ~5k blocker comments + two tiny own comments overflow the
			// 16k cap; eviction must drop the oldest blocker comment(s) first
			// while every own comment survives.
			name: "evicts oldest blocker comments first, keeps own",
			sections: []depSection{
				{label: "B-1 (blocker)", comments: []linear.Comment{
					depComment("BLK1 "+filler(5000), "2026-06-01T00:00:00.000Z"),
					depComment("BLK2 "+filler(5000), "2026-06-02T00:00:00.000Z"),
					depComment("BLK3 "+filler(5000), "2026-06-03T00:00:00.000Z"),
					depComment("BLK4 "+filler(5000), "2026-06-04T00:00:00.000Z"),
				}},
				{label: "I-9 (this issue)", own: true, comments: []linear.Comment{
					depComment("OWNOLD "+filler(50), "2026-07-01T00:00:00.000Z"),
					depComment("OWNNEW "+filler(50), "2026-07-05T00:00:00.000Z"),
				}},
			},
			wantPresent: []string{"BLK4", "OWNOLD", "OWNNEW"},
			wantAbsent:  []string{"BLK1"},
		},
		{
			// A large own section (~15k) plus two ~5k blocker comments forces
			// ALL blocker comments out before the own comment is ever touched.
			name: "evicts every blocker comment before any own comment",
			sections: []depSection{
				{label: "B-1 (blocker)", comments: []linear.Comment{
					depComment("BLK1 "+filler(5000), "2026-06-01T00:00:00.000Z"),
					depComment("BLK2 "+filler(5000), "2026-06-02T00:00:00.000Z"),
				}},
				{label: "I-9 (this issue)", own: true, comments: []linear.Comment{
					depComment("OWNONLY "+filler(15000), "2026-07-01T00:00:00.000Z"),
				}},
			},
			wantPresent: []string{"OWNONLY"},
			wantAbsent:  []string{"BLK1", "BLK2"},
		},
		{
			// An issue with no comments (and no blockers) yields nothing, so
			// spawn.go leaves CLIPSE_DEPENDENCY_NOTES unset.
			name:      "no comments yields empty",
			sections:  []depSection{{label: "I-9 (this issue)", own: true}},
			wantEmpty: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capDependencyNotes(tc.sections)

			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("want empty output, got %d chars:\n%s", len(got), got)
				}
				return
			}
			if len(got) > dependencyNotesCap {
				t.Errorf("len(output) = %d, want <= %d", len(got), dependencyNotesCap)
			}
			for _, s := range tc.wantPresent {
				if !strings.Contains(got, s) {
					t.Errorf("marker %q missing from output, want retained", s)
				}
			}
			for _, s := range tc.wantAbsent {
				if strings.Contains(got, s) {
					t.Errorf("marker %q present in output, want evicted", s)
				}
			}
		})
	}
}

// TestCapDependencyNotes_TruncatesSingleOversizedComment covers the last-resort
// path: one comment alone larger than the cap is truncated to its head (on a
// rune boundary), never dropped to nothing, so the coder still gets its start.
func TestCapDependencyNotes_TruncatesSingleOversizedComment(t *testing.T) {
	// 10k four-byte runes = 40k bytes; cutting at 16k bytes lands mid-rune, so
	// this also guards the rune-boundary back-off.
	big := strings.Repeat("😀", 10000)
	newSections := func() []depSection {
		return []depSection{
			{label: "I-9 (this issue)", own: true, comments: []linear.Comment{
				depComment(big, "2026-07-01T00:00:00.000Z"),
			}},
		}
	}
	full := renderDependencyNotes(newSections())

	got := capDependencyNotes(newSections())

	if got == "" {
		t.Fatal("oversized single comment dropped to empty, want a truncated prefix")
	}
	if len(got) > dependencyNotesCap {
		t.Errorf("len(output) = %d, want <= %d", len(got), dependencyNotesCap)
	}
	if !utf8.ValidString(got) {
		t.Error("truncated output is not valid UTF-8 (a multibyte rune was split)")
	}
	if !strings.HasPrefix(full, got) {
		t.Error("truncated output is not a prefix of the full rendered document")
	}
}
