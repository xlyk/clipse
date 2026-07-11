package cli

import (
	"strings"
	"testing"

	"github.com/xlyk/clipse/internal/boardspec"
)

func TestPlanText(t *testing.T) {
	spec := &boardspec.Spec{Team: "CLI", Issues: []boardspec.Issue{{Ref: "a", Title: "A", Body: "x"}}}
	out := planText(spec, nil)
	if !strings.Contains(out, "+ create") || !strings.Contains(out, "1 create") {
		t.Errorf("expected create row and summary, got:\n%s", out)
	}
}
