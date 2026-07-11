// Package board reconciles a declarative board spec (board.yaml) onto a
// Linear team. It is pure: parsing, validation, content markers, and the
// create/update/skip plan live here with no network or LLM dependency. The
// Linear mutations that execute a plan live in internal/linear/bootstrap,
// walled off from the dispatcher's client.
package board

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Spec is a board.yaml: a Linear team plus the issues to reconcile onto it.
type Spec struct {
	Team          string   `yaml:"team"`
	BaseBranch    string   `yaml:"base_branch"`
	DefaultLabels []string `yaml:"default_labels"`
	Issues        []Issue  `yaml:"issues"`
}

// Issue is one ticket in a Spec. Ref is the stable, tracker-agnostic identity
// used to match a spec entry to a Linear issue across runs. Exactly one of
// Body/BodyFile is set in the file; Parse resolves BodyFile into Body.
type Issue struct {
	Ref       string   `yaml:"ref"`
	Title     string   `yaml:"title"`
	Milestone string   `yaml:"milestone"` // reserved; ignored in v1
	Labels    []string `yaml:"labels"`
	Deps      []string `yaml:"deps"`
	Human     bool     `yaml:"human"`
	Body      string   `yaml:"body"`
	BodyFile  string   `yaml:"body_file"`
}

// Parse reads a board.yaml at path and resolves every issue's BodyFile
// (relative to path's directory) into Body, clearing BodyFile.
func Parse(path string) (*Spec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading spec %s: %w", path, err)
	}
	var spec Spec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	for i := range spec.Issues {
		bf := spec.Issues[i].BodyFile
		if bf == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, bf))
		if err != nil {
			return nil, fmt.Errorf("reading body_file for %q: %w", spec.Issues[i].Ref, err)
		}
		spec.Issues[i].Body = string(body)
		spec.Issues[i].BodyFile = ""
	}
	return &spec, nil
}
