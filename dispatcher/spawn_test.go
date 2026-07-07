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

// TestDispatcher_ShellFor asserts shellFor resolves each lane's shell
// allow-list policy from cfg.Shell, JSON-encoding a restrictive Commands
// list into compact JSON. It mirrors TestDispatcher_ModelsFor's lane
// mapping: Coder gets both its own and the docs sub-step's policy, Reviewer
// gets only its own, everything else (e.g. git_operator, which never spawns
// a DAC worker) gets neither.
func TestDispatcher_ShellFor(t *testing.T) {
	d := &Dispatcher{cfg: config.Config{Shell: config.Shell{
		Coder:     config.ShellPolicy{Commands: []string{"git", "gh"}},
		CoderDocs: config.ShellPolicy{Commands: []string{"cat", "ls"}},
		Reviewer:  config.ShellPolicy{Commands: []string{"git", "gh", "ls"}},
	}}}
	for _, tc := range []struct{ lane, want, wantDocs string }{
		{string(contract.LaneCoder), `["git","gh"]`, `["cat","ls"]`},
		{string(contract.LaneReviewer), `["git","gh","ls"]`, ""},
		{string(contract.LaneGitOperator), "", ""},
	} {
		s, ds := d.shellFor(tc.lane)
		if s != tc.want || ds != tc.wantDocs {
			t.Errorf("shellFor(%q) = (%q,%q), want (%q,%q)", tc.lane, s, ds, tc.want, tc.wantDocs)
		}
	}
}

// TestDispatcher_ShellFor_AllPolicyEncodesEmpty asserts an All policy (the
// decision-2026-07-07 default) encodes as "" rather than as JSON —
// internal/spawn.LocalSpawner's workerArgs must omit
// --shell-allow-list/--docs-shell-allow-list entirely for the default
// deploy, not hand the worker an explicit "true" or "[]" to parse.
func TestDispatcher_ShellFor_AllPolicyEncodesEmpty(t *testing.T) {
	d := &Dispatcher{cfg: config.Config{Shell: config.Shell{
		Coder:     config.ShellPolicy{All: true},
		CoderDocs: config.ShellPolicy{All: true},
		Reviewer:  config.ShellPolicy{All: true},
	}}}
	for _, tc := range []struct{ lane, want, wantDocs string }{
		{string(contract.LaneCoder), "", ""},
		{string(contract.LaneReviewer), "", ""},
	} {
		s, ds := d.shellFor(tc.lane)
		if s != tc.want || ds != tc.wantDocs {
			t.Errorf("shellFor(%q) = (%q,%q), want (%q,%q)", tc.lane, s, ds, tc.want, tc.wantDocs)
		}
	}
}
