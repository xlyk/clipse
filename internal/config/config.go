package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Defaults applied to fields absent from the YAML document.
const (
	defaultPollIntervalS     = 30
	defaultCapsGlobal        = 8
	defaultCapsPerLaneCoder  = 4
	defaultCapsPerLaneReview = 2
	defaultCapsPerLaneGitOp  = 1
	defaultCapsPerLaneScribe = 1
	defaultTurnCap           = 5
	defaultMaxRuntimeS       = 3600
	defaultLaneLabelPrefix   = "agent:"
	defaultMaxAttempts       = 3
)

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
	Scribe      int `yaml:"scribe"`
}

// Caps bounds the dispatch loop's concurrency.
type Caps struct {
	Global  int         `yaml:"global"`
	PerLane PerLaneCaps `yaml:"per_lane"`
}

// Config is the fully parsed and validated clipse configuration.
type Config struct {
	Repo            Repo   `yaml:"repo"`
	PollIntervalS   int    `yaml:"poll_interval_s"`
	Caps            Caps   `yaml:"caps"`
	TurnCap         int    `yaml:"turn_cap"`
	MaxRuntimeS     int    `yaml:"max_runtime_s"`
	LaneLabelPrefix string `yaml:"lane_label_prefix"`
	MaxAttempts     int    `yaml:"max_attempts"`
}

// rawPerLaneCaps mirrors PerLaneCaps with pointer fields so we can tell
// "absent from YAML" (nil, gets a default) apart from "explicitly zero or
// negative" (non-nil, must fail validation).
type rawPerLaneCaps struct {
	Coder       *int `yaml:"coder"`
	Reviewer    *int `yaml:"reviewer"`
	GitOperator *int `yaml:"git_operator"`
	Scribe      *int `yaml:"scribe"`
}

// rawCaps mirrors Caps with a pointer Global field for the same reason.
type rawCaps struct {
	Global  *int           `yaml:"global"`
	PerLane rawPerLaneCaps `yaml:"per_lane"`
}

// rawConfig mirrors Config but uses pointers for every field that has a
// default, so Load can distinguish "missing from YAML" from "explicitly
// set to the zero value" before defaulting and validating.
type rawConfig struct {
	Repo            Repo    `yaml:"repo"`
	PollIntervalS   *int    `yaml:"poll_interval_s"`
	Caps            rawCaps `yaml:"caps"`
	TurnCap         *int    `yaml:"turn_cap"`
	MaxRuntimeS     *int    `yaml:"max_runtime_s"`
	LaneLabelPrefix *string `yaml:"lane_label_prefix"`
	MaxAttempts     *int    `yaml:"max_attempts"`
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

	cfg := &Config{
		Repo:            raw.Repo,
		PollIntervalS:   intOrDefault(raw.PollIntervalS, defaultPollIntervalS),
		TurnCap:         intOrDefault(raw.TurnCap, defaultTurnCap),
		MaxRuntimeS:     intOrDefault(raw.MaxRuntimeS, defaultMaxRuntimeS),
		LaneLabelPrefix: stringOrDefault(raw.LaneLabelPrefix, defaultLaneLabelPrefix),
		MaxAttempts:     intOrDefault(raw.MaxAttempts, defaultMaxAttempts),
		Caps: Caps{
			Global: intOrDefault(raw.Caps.Global, defaultCapsGlobal),
			PerLane: PerLaneCaps{
				Coder:       intOrDefault(raw.Caps.PerLane.Coder, defaultCapsPerLaneCoder),
				Reviewer:    intOrDefault(raw.Caps.PerLane.Reviewer, defaultCapsPerLaneReview),
				GitOperator: intOrDefault(raw.Caps.PerLane.GitOperator, defaultCapsPerLaneGitOp),
				Scribe:      intOrDefault(raw.Caps.PerLane.Scribe, defaultCapsPerLaneScribe),
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
	if cfg.Caps.PerLane.Scribe < 0 {
		return fmt.Errorf("caps.per_lane.scribe must not be negative, got %d", cfg.Caps.PerLane.Scribe)
	}
	return nil
}
