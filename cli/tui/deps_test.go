package tui

import (
	"math"
	"reflect"
	"testing"
)

// TestParseDeps covers the forgiving JSON decode: well-formed arrays parse,
// while empty/blank/malformed values all degrade to no dependencies rather
// than erroring (a garbled deps column must never break rendering).
func TestParseDeps(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty string", "", nil},
		{"empty array", "[]", []string{}},
		{"single", `["a"]`, []string{"a"}},
		{"multiple", `["a","b","c"]`, []string{"a", "b", "c"}},
		{"malformed", `[not json`, nil},
		{"not an array", `{"a":1}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseDeps(tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseDeps(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestUnmetDeps asserts dependency resolution to identifiers of the not-yet-
// terminal deps: done/cancelled deps drop out, unknown deps stay unmet and
// fall back to a short id, and malformed/empty deps resolve to nothing.
func TestUnmetDeps(t *testing.T) {
	identByID := map[string]string{
		"id-done":      "CLI-1",
		"id-cancelled": "CLI-2",
		"id-running":   "CLI-3",
		"id-todo":      "CLI-4",
	}
	statusByID := map[string]string{
		"id-done":      "done",
		"id-cancelled": "cancelled",
		"id-running":   "running",
		"id-todo":      "todo",
	}

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"all terminal → none", `["id-done","id-cancelled"]`, nil},
		{"mixed → only unmet", `["id-done","id-running","id-todo"]`, []string{"CLI-3", "CLI-4"}},
		{"unknown dep → short id", `["deadbeefcafe"]`, []string{"deadbeef"}},
		{"empty", "", nil},
		{"malformed", `[bad`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unmetDeps(tt.raw, identByID, statusByID)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("unmetDeps(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

// TestBlockers asserts every dep is preserved (met or not) with its met flag,
// so the detail view can render a full "blocked-by CLI-8 ✓, CLI-9 ⏳" line.
func TestBlockers(t *testing.T) {
	identByID := map[string]string{"a": "CLI-8", "b": "CLI-9"}
	statusByID := map[string]string{"a": "done", "b": "review"}

	got := blockers(`["a","b"]`, identByID, statusByID)
	want := []blockerState{
		{Identifier: "CLI-8", Met: true},
		{Identifier: "CLI-9", Met: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blockers = %#v, want %#v", got, want)
	}
}

// TestEstimateCostUSD asserts the blended display-rate cost math.
func TestEstimateCostUSD(t *testing.T) {
	// 1M input @ $3 + 1M output @ $15 = $18.00.
	if got := estimateCostUSD(1_000_000, 1_000_000); math.Abs(got-18.0) > 1e-9 {
		t.Errorf("estimateCostUSD(1M,1M) = %f, want 18.0", got)
	}
	if got := estimateCostUSD(0, 0); got != 0 {
		t.Errorf("estimateCostUSD(0,0) = %f, want 0", got)
	}
}
