package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Defaults applied to fields absent from the YAML document.
const (
	defaultPollIntervalS     = 30
	defaultCapsGlobal        = 8
	defaultCapsPerLaneCoder  = 4
	defaultCapsPerLaneReview = 2
	defaultCapsPerLaneGitOp  = 1
	defaultTurnCap           = 5
	defaultMaxRuntimeS       = 3600
	defaultLaneLabelPrefix   = "agent:"
	defaultMaxAttempts       = 3
	// defaultReworkCap is the default amendment-C1 rework_cap: how many
	// times a single issue may cycle review/merging -> rework (the
	// Reviewer lane's changes_requested, or the Git-operator lane's
	// stale-base-conflict route) before the dispatcher parks it in Blocked
	// instead of routing it to rework again, breaking what would otherwise
	// be a possible infinite Coder<->Reviewer loop.
	defaultReworkCap = 3
	// defaultRecoverCap is the default auto-unblock (layer 1) budget: how
	// many times the dispatcher deterministically re-queues a single issue
	// after a *transient* failure (a worker-emitted block_kind=transient, a
	// run-level crash/malformed-result/timeout, or a spawn/workspace failure)
	// before it parks the issue in Blocked for good. Bounded + backed off
	// (see recover_backoff_s), so a genuinely stuck issue retries a fixed
	// number of times and then stops — never a hot loop. Non-transient blocks
	// (capability/needs_input), rework-cap exhaustion, illegal transitions,
	// and orphan max_attempts are NOT covered: those park immediately. A cap
	// of 0 disables auto-recovery entirely (every failure parks).
	defaultRecoverCap = 5
	// defaultMaxTokensPerRun is the per-round context (input-token) ceiling
	// passed to the worker (as --max-tokens) when max_tokens_per_run is
	// absent from the YAML document: the largest single DAC round's input
	// the worker will allow before blocking, with
	// outcome=blocked/block_kind=capability (Phase 2 plan item B2).
	//
	// This is NOT a cumulative-spend cap. drive_turn (agent/) compares each
	// round's input tokens individually, not their running sum, so a long
	// but healthy turn can run indefinitely without ever tripping it — it's
	// a post-compaction runaway guard. DAC's built-in auto-summarizer
	// already keeps each round under a fraction of the model's advertised
	// context window, which clipse tunes down via the coder/reviewer
	// profile's context_window_tokens field (default 200k, so compaction
	// triggers around 170k/round — see AGENTS.md). 400k sits comfortably
	// above that trigger, giving compaction room to do its job, while
	// staying well under any real model's context window, so this guard
	// only fires on a genuine runaway. max_runtime_s and turn_cap remain
	// the backstops against total work across a run (wall-clock,
	// continuation count) — this bounds one round, not the whole run.
	// Override per-repo via max_tokens_per_run.
	defaultMaxTokensPerRun = 400_000
	// defaultCheckpointsDir mirrors defaultBoardDir's default board
	// directory ("./.clipse"): the design doc's checkpointer-path
	// convention is "<board>/checkpoints/<issue_id>.db". This is a plain
	// literal default (not computed from a possibly-overridden BoardDir),
	// so a deployment that overrides board_dir but not checkpoints_dir
	// should set both explicitly.
	defaultCheckpointsDir = "./.clipse/checkpoints"
	// defaultBoardDir matches `clipse dispatch`'s long-standing --board
	// default, now promoted into config so it can be set once instead of
	// passed on every invocation.
	defaultBoardDir = "./.clipse"
	// Keep in sync with the Python profile defaults in
	// agent/src/clipse_agent/profiles/{coder,reviewer}.py — a divergence
	// silently changes an unconfigured deploy's model.
	defaultModelCoder     = "anthropic:claude-sonnet-4-6"
	defaultModelCoderDocs = "anthropic:claude-sonnet-4-6"
	defaultModelReviewer  = "anthropic:claude-opus-4-6"
)

// defaultEnvAllowlist is the env var allow-list applied when env_allowlist
// is absent from the YAML document: what a Phase-2 DAC worker needs
// (ANTHROPIC_API_KEY, PATH, HOME, a scoped gh token under either common var
// name), plus TESTWORKER_SCENARIO so the kernel's fake-worker test harness
// keeps working. Never LINEAR_API_KEY — see validate.
var defaultEnvAllowlist = []string{
	"ANTHROPIC_API_KEY",
	"PATH",
	"HOME",
	"GH_TOKEN",
	"GITHUB_TOKEN",
	"TESTWORKER_SCENARIO",
}

// Repo describes the single repository clipse manages.
type Repo struct {
	Remote     string `yaml:"remote"`
	Path       string `yaml:"path"`
	BaseBranch string `yaml:"base_branch"`
}

// PerLaneCaps caps concurrent workers per lane.
type PerLaneCaps struct {
	Coder       int `yaml:"coder"`
	Reviewer    int `yaml:"reviewer"`
	GitOperator int `yaml:"git_operator"`
}

// Caps bounds the dispatch loop's concurrency.
type Caps struct {
	Global  int         `yaml:"global"`
	PerLane PerLaneCaps `yaml:"per_lane"`
}

// Worker configures how the dispatcher execs the clipse-worker subprocess.
type Worker struct {
	// Command is the argv PREFIX the Spawner (internal/spawn.LocalSpawner)
	// execs for every worker invocation, e.g.
	//   ["uv", "--project", "/abs/path/agent", "run", "clipse-worker"]
	// clipse-worker is a uv console script defined in agent/pyproject.toml,
	// so the common case runs it scoped to that project rather than
	// requiring a global install. The Spawner appends the per-run flags
	// (--issue/--lane/--run/--thread/--workspace/--checkpoint-db/
	// --max-tokens) after this prefix — see internal/spawn.WorkerSpec.
	//
	// Machine-specific (an absolute --project path in the common case), so
	// it has no default: validate rejects an empty Command.
	Command []string `yaml:"command"`
}

// Models specifies the "provider:model" spec used per DAC profile
// (validateModelSpec enforces the "provider:model", both-halves-non-empty
// shape only — the kernel stays LLM-free and never validates a model
// vocabulary). CoderDocs is the Coder lane's documentation sub-step
// (graphs/coder.py's run_docs node), not a separate lane.
type Models struct {
	Coder     string `yaml:"coder"`
	CoderDocs string `yaml:"coder_docs"`
	Reviewer  string `yaml:"reviewer"`
}

// ModelParams holds opaque per-lane model-construction kwargs (DAC
// create_model extra_kwargs — e.g. a codex reasoning effort or an anthropic
// extended-thinking budget), threaded down to the worker. Unlike Models, the
// kernel never validates the *semantics* of these values — they are carried
// through as-is, keeping the kernel LLM-free. A nil map means the lane has no
// overrides (inherit ~/.deepagents/config.toml / provider default); there is
// no defaulting.
type ModelParams struct {
	Coder     map[string]any `yaml:"coder"`
	CoderDocs map[string]any `yaml:"coder_docs"`
	Reviewer  map[string]any `yaml:"reviewer"`
}

// Config is the fully parsed and validated clipse configuration.
type Config struct {
	Repo Repo `yaml:"repo"`
	// TeamKey is the Linear team key (e.g. "CLI") the candidate-issues query
	// filters to, so the dispatcher only ever considers issues on the team
	// it manages.
	TeamKey string `yaml:"team_key"`
	// TeamID is the Linear team id (UUID) used to resolve a board Column to
	// that team's Linear workflow-state id for SetState (see
	// internal/linear/state_resolver.go).
	TeamID          string `yaml:"team_id"`
	PollIntervalS   int    `yaml:"poll_interval_s"`
	Caps            Caps   `yaml:"caps"`
	TurnCap         int    `yaml:"turn_cap"`
	MaxRuntimeS     int    `yaml:"max_runtime_s"`
	LaneLabelPrefix string `yaml:"lane_label_prefix"`
	MaxAttempts     int    `yaml:"max_attempts"`
	// ReworkCap bounds how many times a single issue may cycle into rework
	// (amendment C1) — see issues.rework_count (internal/store) and
	// dispatcher.blockIfReworkCapExceeded, which parks the issue in
	// Blocked instead of transitioning to rework once incrementing
	// rework_count would exceed this. Defaults to defaultReworkCap when
	// absent from the YAML document.
	ReworkCap int `yaml:"rework_cap"`
	// RecoverCap bounds auto-unblock layer 1: how many times the dispatcher
	// deterministically re-queues a single issue after a transient failure
	// before parking it in Blocked — see issues.recover_attempts
	// (internal/store) and dispatcher.parkOrRetry. Defaults to
	// defaultRecoverCap when absent; a value of 0 disables auto-recovery (every
	// failure parks immediately, the pre-layer-1 behavior).
	RecoverCap int `yaml:"recover_cap"`
	// RecoverBackoffS is the delay (seconds) before a re-queued issue becomes
	// claimable again (issues.blocked_until = now + RecoverBackoffS); the
	// backoff is what makes the retry budget a real anti-hot-loop guard rather
	// than an immediate re-claim. Defaults to the resolved PollIntervalS when
	// absent, so a retry lands roughly one poll later.
	RecoverBackoffS int `yaml:"recover_backoff_s"`
	// Worker configures the clipse-worker subprocess invocation. Required —
	// see Worker.Command's doc comment.
	Worker Worker `yaml:"worker"`
	// Models holds the per-lane "provider:model" spec threaded down to the
	// worker (--model/--docs-model). Each field defaults independently
	// (defaultModelCoder/defaultModelCoderDocs/defaultModelReviewer) when
	// absent from the YAML document.
	Models Models `yaml:"models"`
	// ModelParams holds opaque per-lane model-construction kwargs threaded
	// down to the worker alongside Models. Each field is nil unless the
	// corresponding YAML key is present — no defaulting (see ModelParams's
	// doc comment).
	ModelParams ModelParams `yaml:"model_params"`
	// MaxTokensPerRun is the per-round context (input-token) ceiling passed
	// to the worker (--max-tokens): the largest single DAC round's input
	// the worker will allow before blocking capability — not a
	// cumulative-spend cap. See defaultMaxTokensPerRun's doc comment for
	// the full semantics and its relationship to DAC's auto-compaction and
	// the max_runtime_s/turn_cap backstops. Defaults to
	// defaultMaxTokensPerRun when absent from the YAML document.
	MaxTokensPerRun int `yaml:"max_tokens_per_run"`
	// CheckpointsDir holds one LangGraph checkpointer SQLite database per
	// issue (<CheckpointsDir>/<issue-identifier>.db), outside any git
	// worktree — see dispatcher.checkpointDBPath. Defaults to
	// defaultCheckpointsDir when absent from the YAML document.
	CheckpointsDir string `yaml:"checkpoints_dir"`
	// BoardDir is the dispatcher's state directory: kernel SQLite db,
	// singleton lockfile, per-issue worker stderr logs, and git worktrees.
	// Overridable per-invocation via `clipse dispatch --board`; defaults to
	// defaultBoardDir when absent from the YAML document.
	BoardDir string `yaml:"board_dir"`
	// EnvAllowlist names the environment variables copied from the
	// dispatcher's own process environment into a spawned worker's (see
	// internal/spawn.AllowlistedEnv and the dispatcher's default
	// WithEnvFor). Anything not named here — most importantly
	// LINEAR_API_KEY, the kernel-only Linear credential — is never
	// forwarded to a worker, regardless of what the dispatcher process
	// itself holds. validate rejects LINEAR_API_KEY appearing here (design
	// doc threat model, B3: a worker's shell-enabled DAC agent runs against
	// untrusted Linear issue content, so it must never hold kernel secrets
	// it doesn't need).
	EnvAllowlist []string `yaml:"env_allowlist"`
}

// rawPerLaneCaps mirrors PerLaneCaps with pointer fields so we can tell
// "absent from YAML" (nil, gets a default) apart from "explicitly zero or
// negative" (non-nil, must fail validation).
type rawPerLaneCaps struct {
	Coder       *int `yaml:"coder"`
	Reviewer    *int `yaml:"reviewer"`
	GitOperator *int `yaml:"git_operator"`
}

// rawCaps mirrors Caps with a pointer Global field for the same reason.
type rawCaps struct {
	Global  *int           `yaml:"global"`
	PerLane rawPerLaneCaps `yaml:"per_lane"`
}

// rawModels mirrors Models with pointer fields so Load can distinguish
// "absent from YAML" (nil, gets a default) from "explicitly set" — the same
// reasoning as rawPerLaneCaps. Like rawCaps.PerLane, this is embedded as a
// plain value (not a pointer) in rawConfig: a YAML document that omits
// `models:` entirely still yields a zero-value rawModels with all-nil
// fields, so no nil-sub-struct guard is needed before reading them.
type rawModels struct {
	Coder     *string `yaml:"coder"`
	CoderDocs *string `yaml:"coder_docs"`
	Reviewer  *string `yaml:"reviewer"`
}

// rawModelParams mirrors ModelParams. Unlike rawModels, its fields need no
// pointer indirection to tell "absent from YAML" apart from "explicitly
// set": a map's own zero value (nil) already means absent (yaml.v3 leaves it
// nil unless the document sets the key), and there is no default value to
// distinguish it from (model_params never defaults — see ModelParams's doc
// comment). Like rawModels, this is embedded as a plain value (not a
// pointer) in rawConfig: a YAML document that omits `model_params:` entirely
// still yields a zero-value rawModelParams with all-nil fields, so no
// nil-sub-struct guard is needed before reading them.
type rawModelParams struct {
	Coder     map[string]any `yaml:"coder"`
	CoderDocs map[string]any `yaml:"coder_docs"`
	Reviewer  map[string]any `yaml:"reviewer"`
}

// rawConfig mirrors Config but uses pointers for every field that has a
// default, so Load can distinguish "missing from YAML" from "explicitly
// set to the zero value" before defaulting and validating. EnvAllowlist is a
// plain slice rather than a pointer: a slice's own zero value (nil) already
// means "absent from YAML" (yaml.v3 leaves it nil unless the document sets
// it), and an explicit-but-empty list is a validate-time error either way
// (see validate), so no pointer indirection is needed to tell the two apart.
type rawConfig struct {
	Repo    Repo   `yaml:"repo"`
	TeamKey string `yaml:"team_key"`
	TeamID  string `yaml:"team_id"`
	// Worker is plain (not pointer-wrapped) like Repo: it's required, with
	// no default to apply, so Load copies it straight through and validate
	// checks it.
	Worker Worker `yaml:"worker"`
	// Models is plain (not pointer-wrapped), like Caps: its own fields
	// (rawModels) carry the pointers needed to detect "absent from YAML".
	Models rawModels `yaml:"models"`
	// ModelParams is plain (not pointer-wrapped), like Models: its own
	// fields (rawModelParams) are maps whose nil zero-value already means
	// "absent from YAML" — see rawModelParams's doc comment.
	ModelParams     rawModelParams `yaml:"model_params"`
	PollIntervalS   *int           `yaml:"poll_interval_s"`
	Caps            rawCaps        `yaml:"caps"`
	TurnCap         *int           `yaml:"turn_cap"`
	MaxRuntimeS     *int           `yaml:"max_runtime_s"`
	LaneLabelPrefix *string        `yaml:"lane_label_prefix"`
	MaxAttempts     *int           `yaml:"max_attempts"`
	ReworkCap       *int           `yaml:"rework_cap"`
	RecoverCap      *int           `yaml:"recover_cap"`
	RecoverBackoffS *int           `yaml:"recover_backoff_s"`
	MaxTokensPerRun *int           `yaml:"max_tokens_per_run"`
	CheckpointsDir  *string        `yaml:"checkpoints_dir"`
	BoardDir        *string        `yaml:"board_dir"`
	EnvAllowlist    []string       `yaml:"env_allowlist"`
}

// Load reads the clipse config file at path, applies defaults for fields
// absent from the YAML document, and validates the result. It returns a
// wrapped error naming the offending field on any invalid config.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	// Resolve poll_interval_s first: recover_backoff_s defaults to it (a retry
	// lands roughly one poll later), so it must be known before defaulting.
	pollIntervalS := intOrDefault(raw.PollIntervalS, defaultPollIntervalS)

	cfg := &Config{
		Repo:    raw.Repo,
		TeamKey: raw.TeamKey,
		TeamID:  raw.TeamID,
		Worker:  raw.Worker,
		Models: Models{
			Coder:     stringOrDefault(raw.Models.Coder, defaultModelCoder),
			CoderDocs: stringOrDefault(raw.Models.CoderDocs, defaultModelCoderDocs),
			Reviewer:  stringOrDefault(raw.Models.Reviewer, defaultModelReviewer),
		},
		// model_params is an opaque passthrough (kernel LLM-free, no
		// validated vocabulary) with no defaulting: every lane stays nil
		// unless the YAML document sets it.
		ModelParams: ModelParams{
			Coder:     raw.ModelParams.Coder,
			CoderDocs: raw.ModelParams.CoderDocs,
			Reviewer:  raw.ModelParams.Reviewer,
		},
		PollIntervalS:   pollIntervalS,
		TurnCap:         intOrDefault(raw.TurnCap, defaultTurnCap),
		MaxRuntimeS:     intOrDefault(raw.MaxRuntimeS, defaultMaxRuntimeS),
		LaneLabelPrefix: stringOrDefault(raw.LaneLabelPrefix, defaultLaneLabelPrefix),
		MaxAttempts:     intOrDefault(raw.MaxAttempts, defaultMaxAttempts),
		ReworkCap:       intOrDefault(raw.ReworkCap, defaultReworkCap),
		RecoverCap:      intOrDefault(raw.RecoverCap, defaultRecoverCap),
		RecoverBackoffS: intOrDefault(raw.RecoverBackoffS, pollIntervalS),
		MaxTokensPerRun: intOrDefault(raw.MaxTokensPerRun, defaultMaxTokensPerRun),
		CheckpointsDir:  stringOrDefault(raw.CheckpointsDir, defaultCheckpointsDir),
		BoardDir:        stringOrDefault(raw.BoardDir, defaultBoardDir),
		EnvAllowlist:    stringSliceOrDefault(raw.EnvAllowlist, defaultEnvAllowlist),
		Caps: Caps{
			Global: intOrDefault(raw.Caps.Global, defaultCapsGlobal),
			PerLane: PerLaneCaps{
				Coder:       intOrDefault(raw.Caps.PerLane.Coder, defaultCapsPerLaneCoder),
				Reviewer:    intOrDefault(raw.Caps.PerLane.Reviewer, defaultCapsPerLaneReview),
				GitOperator: intOrDefault(raw.Caps.PerLane.GitOperator, defaultCapsPerLaneGitOp),
			},
		},
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config %s: %w", path, err)
	}

	return cfg, nil
}

func intOrDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

func stringOrDefault(v *string, def string) string {
	if v == nil || *v == "" {
		return def
	}
	return *v
}

// stringSliceOrDefault returns def when v is nil (absent from YAML),
// otherwise v unchanged — including when v is non-nil but empty, which
// validate rejects rather than silently defaulting, so an explicit
// `env_allowlist: []` surfaces as a config error instead of being masked.
func stringSliceOrDefault(v []string, def []string) []string {
	if v == nil {
		return def
	}
	return v
}

func validate(cfg *Config) error {
	if cfg.Repo.Remote == "" {
		return fmt.Errorf("repo.remote is required")
	}
	if cfg.Repo.Path == "" {
		return fmt.Errorf("repo.path is required")
	}
	if cfg.Repo.BaseBranch == "" {
		return fmt.Errorf("repo.base_branch is required")
	}
	if cfg.PollIntervalS <= 0 {
		return fmt.Errorf("poll_interval_s must be positive, got %d", cfg.PollIntervalS)
	}
	if cfg.TurnCap <= 0 {
		return fmt.Errorf("turn_cap must be positive, got %d", cfg.TurnCap)
	}
	if cfg.MaxRuntimeS <= 0 {
		return fmt.Errorf("max_runtime_s must be positive, got %d", cfg.MaxRuntimeS)
	}
	if cfg.MaxAttempts < 1 {
		return fmt.Errorf("max_attempts must be at least 1, got %d", cfg.MaxAttempts)
	}
	if cfg.ReworkCap < 1 {
		return fmt.Errorf("rework_cap must be at least 1, got %d", cfg.ReworkCap)
	}
	if cfg.Caps.Global < 1 {
		return fmt.Errorf("caps.global must be at least 1, got %d", cfg.Caps.Global)
	}
	if cfg.Caps.PerLane.Coder < 0 {
		return fmt.Errorf("caps.per_lane.coder must not be negative, got %d", cfg.Caps.PerLane.Coder)
	}
	if cfg.Caps.PerLane.Reviewer < 0 {
		return fmt.Errorf("caps.per_lane.reviewer must not be negative, got %d", cfg.Caps.PerLane.Reviewer)
	}
	if cfg.Caps.PerLane.GitOperator < 0 {
		return fmt.Errorf("caps.per_lane.git_operator must not be negative, got %d", cfg.Caps.PerLane.GitOperator)
	}
	if len(cfg.EnvAllowlist) == 0 {
		return fmt.Errorf("env_allowlist must not be empty")
	}
	for _, key := range cfg.EnvAllowlist {
		if key == "" {
			return fmt.Errorf("env_allowlist must not contain empty entries")
		}
		// The worker must NEVER see LINEAR_API_KEY — it's the kernel-only
		// Linear credential (design doc threat model, B3). Reject it here
		// rather than let it silently ride along into every worker's env.
		if key == "LINEAR_API_KEY" {
			return fmt.Errorf("env_allowlist must not include LINEAR_API_KEY (kernel-only secret, never passed to a worker)")
		}
	}
	// team_key/team_id scope every Linear query/mutation the dispatcher
	// issues (candidate-issues filter, workflow-state resolution for
	// SetState — see internal/linear/state_resolver.go); without them the
	// dispatcher can't safely talk to Linear at all.
	if cfg.TeamKey == "" {
		return fmt.Errorf("team_key is required")
	}
	if cfg.TeamID == "" {
		return fmt.Errorf("team_id is required")
	}
	if cfg.MaxTokensPerRun <= 0 {
		return fmt.Errorf("max_tokens_per_run must be positive, got %d", cfg.MaxTokensPerRun)
	}
	// recover_cap/recover_backoff_s bound auto-unblock layer 1. Both are
	// non-negative (unlike max_attempts/rework_cap, which must be >= 1): a
	// recover_cap of 0 is a valid kill switch that disables auto-recovery, and
	// a recover_backoff_s of 0 means "re-claimable on the next tick" (no
	// backoff). Negative values are meaningless, so reject them.
	if cfg.RecoverCap < 0 {
		return fmt.Errorf("recover_cap must not be negative, got %d", cfg.RecoverCap)
	}
	if cfg.RecoverBackoffS < 0 {
		return fmt.Errorf("recover_backoff_s must not be negative, got %d", cfg.RecoverBackoffS)
	}
	// worker.command is the argv prefix the Spawner execs for every worker
	// invocation; it's machine-specific (an absolute --project path in the
	// common case), so there is no default to fall back to here. Checked
	// last (appended after every pre-existing check, matching how
	// env_allowlist's and team_key/team_id's own checks were each appended
	// in turn) so it never shadows an earlier-positioned check's own test
	// fixture — those fixtures only ever set the one field under test and
	// deliberately omit worker.command, since it predates them.
	if len(cfg.Worker.Command) == 0 {
		return fmt.Errorf("worker.command is required")
	}
	for _, arg := range cfg.Worker.Command {
		if arg == "" {
			return fmt.Errorf("worker.command must not contain empty entries")
		}
	}
	// models.* are checked last, alongside worker.command, for the same
	// isolation reason: a fixture targeting a single models.* error carries
	// a full valid repo/team_key/team_id/worker.command, so nothing earlier
	// in this function fires first.
	if err := validateModelSpec("models.coder", cfg.Models.Coder); err != nil {
		return err
	}
	if err := validateModelSpec("models.coder_docs", cfg.Models.CoderDocs); err != nil {
		return err
	}
	if err := validateModelSpec("models.reviewer", cfg.Models.Reviewer); err != nil {
		return err
	}
	return nil
}

// validateModelSpec checks only the "provider:model" shape (both halves
// non-empty) — never a model vocabulary, keeping the kernel LLM-free by
// design.
func validateModelSpec(field, spec string) error {
	provider, name, ok := strings.Cut(spec, ":")
	if !ok || provider == "" || name == "" {
		return fmt.Errorf("%s must be \"provider:model\" with both halves non-empty, got %q", field, spec)
	}
	return nil
}
