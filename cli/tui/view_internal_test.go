package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPaletteAdaptive pins the adaptive palette (P5): every color must carry
// the original GitHub-dark hex as its Dark variant (so dark terminals are
// pixel-identical to v1) and a distinct, non-empty GitHub-light analogue as
// Light. A missing Light value would silently render the dark hex on light
// backgrounds — exactly the failure mode AdaptiveColor exists to prevent.
func TestPaletteAdaptive(t *testing.T) {
	tests := []struct {
		name string
		c    lipgloss.AdaptiveColor
		dark string // the original v1 dark-palette hex, pinned
	}{
		{"cText", cText, "#c9d1d9"},
		{"cDim", cDim, "#6e7681"},
		{"cBorder", cBorder, "#30363d"},
		{"cInk", cInk, "#0d1117"},
		{"cGreen", cGreen, "#3fb950"},
		{"cCyan", cCyan, "#58a6ff"},
		{"cRed", cRed, "#f85149"},
		{"cAmber", cAmber, "#d29922"},
		{"cPurple", cPurple, "#bc8cff"},
		{"cTeal", cTeal, "#39c5cf"},
		{"cOrange", cOrange, "#db6d28"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.c.Dark != tt.dark {
				t.Errorf("%s.Dark = %q, want the original dark hex %q", tt.name, tt.c.Dark, tt.dark)
			}
			if tt.c.Light == "" {
				t.Errorf("%s.Light is empty, want a GitHub-light analogue", tt.name)
			}
			if tt.c.Light == tt.c.Dark {
				t.Errorf("%s.Light == Dark (%q), want a distinct light-mode value", tt.name, tt.c.Light)
			}
		})
	}
}

// TestLaneColorAdaptive asserts the lane-identity mapping survives the type
// change: each lane keeps its established dark hue.
func TestLaneColorAdaptive(t *testing.T) {
	tests := []struct {
		lane string
		dark string
	}{
		{"coder", "#58a6ff"},
		{"reviewer", "#bc8cff"},
		{"git_operator", "#db6d28"},
		{"unknown", "#6e7681"},
	}
	for _, tt := range tests {
		if got := laneColor(tt.lane); got.Dark != tt.dark {
			t.Errorf("laneColor(%q).Dark = %q, want %q", tt.lane, got.Dark, tt.dark)
		}
	}
}
