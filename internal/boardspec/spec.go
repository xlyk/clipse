// Package boardspec reconciles a declarative board spec (board.yaml) onto a
// Linear team. It is pure: parsing, validation, content markers, and the
// create/update/skip plan live here with no network or LLM dependency. The
// Linear mutations that execute a plan live in internal/linear/bootstrap,
// walled off from the dispatcher's client.
package boardspec

import (
	"bytes"
	"fmt"
	"io"
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
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("parsing spec %s: multiple YAML documents are not supported", path)
		}
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	bodySources, err := issueBodySources(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing spec %s: %w", path, err)
	}
	if len(bodySources) != len(spec.Issues) {
		return nil, fmt.Errorf("parsing spec %s: issue source count mismatch", path)
	}
	for i, source := range bodySources {
		if source.body == source.bodyFile {
			return nil, fmt.Errorf("parsing spec %s: issue %q must set exactly one of body or body_file", path, spec.Issues[i].Ref)
		}
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

type bodySource struct {
	body     bool
	bodyFile bool
}

// issueBodySources records which mutually-exclusive body keys appeared in the
// YAML before body_file is resolved into Body. The resolved Spec cannot retain
// this distinction because both sources intentionally converge on Issue.Body.
func issueBodySources(raw []byte) ([]bodySource, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || len(doc.Content[0].Content) == 0 {
		return nil, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("document root must be a mapping")
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "issues" {
			continue
		}
		issues := root.Content[i+1]
		if issues.Kind != yaml.SequenceNode {
			return nil, fmt.Errorf("issues must be a sequence")
		}
		out := make([]bodySource, len(issues.Content))
		for issueIndex, issue := range issues.Content {
			if issue.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("issue %d must be a mapping", issueIndex+1)
			}
			for j := 0; j+1 < len(issue.Content); j += 2 {
				switch issue.Content[j].Value {
				case "body":
					out[issueIndex].body = true
				case "body_file":
					out[issueIndex].bodyFile = true
				}
			}
		}
		return out, nil
	}
	return nil, nil
}
