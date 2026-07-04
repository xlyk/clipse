package dispatcher

import (
	"testing"

	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/contract"
)

// TestDispatcher_ModelsFor asserts modelsFor resolves each lane's
// "provider:model" spec from cfg.Models: the Coder lane gets its own model
// plus the docs sub-step's model, the Reviewer lane gets its model and no
// docs model (the docs step only runs inside the coder graph, never the
// reviewer's), and any other lane (e.g. git_operator, which never spawns a
// DAC worker) gets neither.
func TestDispatcher_ModelsFor(t *testing.T) {
	d := &Dispatcher{cfg: config.Config{Models: config.Models{
		Coder: "openai_codex:gpt-5.5", CoderDocs: "anthropic:claude-sonnet-4-6", Reviewer: "anthropic:claude-opus-4-6",
	}}}
	for _, tc := range []struct{ lane, wantModel, wantDocs string }{
		{string(contract.LaneCoder), "openai_codex:gpt-5.5", "anthropic:claude-sonnet-4-6"},
		{string(contract.LaneReviewer), "anthropic:claude-opus-4-6", ""},
		{string(contract.LaneGitOperator), "", ""},
	} {
		m, dm := d.modelsFor(tc.lane)
		if m != tc.wantModel || dm != tc.wantDocs {
			t.Errorf("modelsFor(%q) = (%q,%q), want (%q,%q)", tc.lane, m, dm, tc.wantModel, tc.wantDocs)
		}
	}
}
