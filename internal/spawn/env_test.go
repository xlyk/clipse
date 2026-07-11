package spawn_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/spawn"
)

// TestAllowlistedEnv covers the env-scrubbing mechanism (threat model B3):
// AllowlistedEnv is what stands between a spawned worker and the
// dispatcher's full process environment, so it must forward exactly the
// allow-listed keys that are actually set — nothing else, and never
// LINEAR_API_KEY even if a caller's allowlist names it by mistake.
func TestAllowlistedEnv(t *testing.T) {
	tests := []struct {
		name      string
		environ   []string
		allowlist []string
		wantEnv   []string // exact expected entries, in environ's order
	}{
		{
			name: "only allowlisted keys present in environ pass through",
			environ: []string{
				"LINEAR_API_KEY=super-secret",
				"ANTHROPIC_API_KEY=sk-abc",
				"PATH=/usr/bin",
				"HOME=/home/x",
				"RANDOM_VAR=z",
			},
			allowlist: []string{"ANTHROPIC_API_KEY", "PATH", "HOME", "GH_TOKEN"},
			wantEnv:   []string{"ANTHROPIC_API_KEY=sk-abc", "PATH=/usr/bin", "HOME=/home/x"},
		},
		{
			name:      "allowlisted key absent from environ is omitted, not inserted empty",
			environ:   []string{"PATH=/usr/bin"},
			allowlist: []string{"PATH", "GH_TOKEN"},
			wantEnv:   []string{"PATH=/usr/bin"},
		},
		{
			name:      "TESTWORKER_SCENARIO passes through when allowlisted and set (kernel test harness)",
			environ:   []string{"TESTWORKER_SCENARIO=hang", "PATH=/usr/bin"},
			allowlist: []string{"PATH", "TESTWORKER_SCENARIO"},
			wantEnv:   []string{"TESTWORKER_SCENARIO=hang", "PATH=/usr/bin"},
		},
		{
			name:      "LINEAR_API_KEY is never forwarded, even if mistakenly allowlisted",
			environ:   []string{"LINEAR_API_KEY=super-secret", "PATH=/usr/bin"},
			allowlist: []string{"LINEAR_API_KEY", "PATH"},
			wantEnv:   []string{"PATH=/usr/bin"},
		},
		{
			name:      "empty allowlist yields empty env",
			environ:   []string{"PATH=/usr/bin", "HOME=/home/x"},
			allowlist: nil,
			wantEnv:   nil,
		},
		{
			name:      "malformed environ entries without '=' are ignored",
			environ:   []string{"PATH=/usr/bin", "MALFORMED"},
			allowlist: []string{"PATH", "MALFORMED"},
			wantEnv:   []string{"PATH=/usr/bin"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := spawn.AllowlistedEnv(tt.environ, tt.allowlist)
			if !slices.Equal(got, tt.wantEnv) {
				t.Errorf("AllowlistedEnv(...) = %v, want %v", got, tt.wantEnv)
			}
			for _, kv := range got {
				if strings.HasPrefix(kv, "LINEAR_API_KEY=") {
					t.Errorf("AllowlistedEnv(...) = %v, must never contain LINEAR_API_KEY", got)
				}
			}
		})
	}
}

func TestMergeEnv_OverlaysByNameWithoutDuplicates(t *testing.T) {
	base := []string{
		"ANTHROPIC_API_KEY=model",
		"PATH=/normal/bin",
		"HOME=/normal/home",
		"CLIPSE_ISSUE_TEXT=task",
		"PATH=/stale/duplicate",
	}
	overlay := []string{
		"PATH=/host/bin",
		"HOME=/host/home",
		"DAYTONA_API_KEY=daytona",
		"DAYTONA_API_URL=https://daytona.example",
		"DAYTONA_TARGET=us",
	}
	want := []string{
		"ANTHROPIC_API_KEY=model",
		"PATH=/host/bin",
		"HOME=/host/home",
		"CLIPSE_ISSUE_TEXT=task",
		"DAYTONA_API_KEY=daytona",
		"DAYTONA_API_URL=https://daytona.example",
		"DAYTONA_TARGET=us",
	}
	if got := spawn.MergeEnv(base, overlay); !slices.Equal(got, want) {
		t.Fatalf("MergeEnv() = %v, want %v", got, want)
	}
}
