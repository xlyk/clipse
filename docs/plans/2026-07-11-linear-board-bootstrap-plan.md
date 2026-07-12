# Linear Board Bootstrap Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `clipse board plan|apply` — a deterministic tool that reconciles a structured `board.yaml` onto a Linear team idempotently — plus a clipse-specific authoring skill that produces the spec from a prose plan.

**Architecture:** A pure spec package (`internal/board`: parse/validate/marker/plan, no network) diffs a `board.yaml` against the team's current issues (matched by a hidden `<!-- clipse-ref: … sha:… -->` marker) and emits a plan; a walled-off Linear mutation client (`internal/linear/bootstrap`) applies it. The dispatcher's `HTTPClient` never gains create/delete. Design: `docs/design/2026-07-11-linear-board-bootstrap.md`.

**Tech Stack:** Go 1.25, `gopkg.in/yaml.v3` (existing dep), `spf13/cobra`, stdlib `net/http`/`crypto/sha256`/`testing`/`httptest`. No new dependencies.

## Global Constraints

- Go: check and wrap every error (`fmt.Errorf("…: %w", err)`); no swallowed errors. Runtime/daemon logs use `log/slog` JSON only; a CLI command *result* on stdout is fine.
- Tests: standard-library `testing` only (no testify); table-driven; interfaces at the consumption site. Zero real network — only `httptest` loopback. Zero LLM.
- The dispatcher client (`internal/linear.HTTPClient`) must NOT gain issue create/delete. Board mutations live only in `internal/linear/bootstrap`.
- `LINEAR_API_KEY` stays kernel/operator-only; never added to `env_allowlist`. `clipse board` reads it directly like `clipse dispatch`.
- No LLM anywhere in Go. The prose→spec step is the skill only.
- Commits: Conventional Commits, lowercase, no trailing period, no AI/Claude signature. One concern per commit. Stage exact files (never `git add -A`/`.`).
- The board spec struct uses `yaml` tags parsed by `yaml.v3`. `schema/board-spec.schema.json` is a reference artifact — not codegen'd, not loaded at runtime.

---

## Phase 1 — spec core (pure, no network)

### Task 1: Board spec types + YAML parse

**Files:**
- Create: `internal/board/spec.go`
- Test: `internal/board/spec_test.go`
- Create: `internal/board/testdata/simple.yaml`
- Create: `internal/board/testdata/bodies/cfg-1.md`

**Interfaces:**
- Produces: `type Spec struct{ Team string; BaseBranch string; DefaultLabels []string; Issues []Issue }`;
  `type Issue struct{ Ref, Title, Milestone string; Labels, Deps []string; Human bool; Body, BodyFile string }`;
  `func Parse(path string) (*Spec, error)` — reads YAML, then resolves each `BodyFile` (relative to the spec's dir) into `Body`, returning a `*Spec` whose every `Issue.Body` is populated and `BodyFile` cleared.

- [ ] **Step 1: Write the failing test**

```go
package board

import "testing"

func TestParseResolvesBodyFile(t *testing.T) {
	spec, err := Parse("testdata/simple.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if spec.Team != "CLI" {
		t.Errorf("Team = %q, want CLI", spec.Team)
	}
	if len(spec.Issues) != 2 {
		t.Fatalf("Issues = %d, want 2", len(spec.Issues))
	}
	if got := spec.Issues[0].Body; got != "inline core body\n" {
		t.Errorf("inline body = %q", got)
	}
	// cfg-1 uses body_file; Parse must have loaded and cleared it.
	if got := spec.Issues[1].Body; got != "config from file\n" {
		t.Errorf("file body = %q", got)
	}
	if spec.Issues[1].BodyFile != "" {
		t.Errorf("BodyFile not cleared: %q", spec.Issues[1].BodyFile)
	}
}
```

- [ ] **Step 2: Create fixtures**

`internal/board/testdata/simple.yaml`:
```yaml
team: CLI
default_labels: [agent:coder]
issues:
  - ref: core-1
    title: Greeter core
    deps: []
    body: |
      inline core body
  - ref: cfg-1
    title: Config dataclass
    deps: [core-1]
    body_file: bodies/cfg-1.md
```

`internal/board/testdata/bodies/cfg-1.md`:
```
config from file
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/board/ -run TestParseResolvesBodyFile`
Expected: FAIL — `undefined: Parse`.

- [ ] **Step 4: Implement `spec.go`**

```go
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
// (relative to path's directory) into Body.
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
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/board/ -run TestParseResolvesBodyFile`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/board/spec.go internal/board/spec_test.go internal/board/testdata
git commit -m "feat(board): parse board.yaml spec with body_file resolution"
```

### Task 2: Spec validation

**Files:**
- Create: `internal/board/validate.go`
- Test: `internal/board/validate_test.go`

**Interfaces:**
- Consumes: `Spec`, `Issue` (Task 1).
- Produces: `func (s *Spec) Validate() error` — returns a single error joining every problem found (`errors.Join`). Checks: `Team` non-empty; each `Ref`/`Title` non-empty; `Ref` unique; each `Deps` entry references a defined `Ref`; the `Deps` graph is acyclic; exactly one of `Body`/`BodyFile` originally set (post-Parse: `Body` non-empty); every label non-empty. Also `func (s *Spec) TopoOrder() ([]int, error)` returning issue indices in dependency order (blockers first), used by the applier.

- [ ] **Step 1: Write the failing tests**

```go
package board

import (
	"strings"
	"testing"
)

func specWith(issues ...Issue) *Spec { return &Spec{Team: "CLI", Issues: issues} }

func TestValidateRejectsDuplicateRef(t *testing.T) {
	s := specWith(
		Issue{Ref: "a", Title: "A", Body: "x"},
		Issue{Ref: "a", Title: "A2", Body: "y"},
	)
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate ref") {
		t.Fatalf("want duplicate ref error, got %v", err)
	}
}

func TestValidateRejectsUndefinedDep(t *testing.T) {
	s := specWith(Issue{Ref: "a", Title: "A", Body: "x", Deps: []string{"ghost"}})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("want undefined dep error, got %v", err)
	}
}

func TestValidateRejectsCycle(t *testing.T) {
	s := specWith(
		Issue{Ref: "a", Title: "A", Body: "x", Deps: []string{"b"}},
		Issue{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
	)
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("want cycle error, got %v", err)
	}
}

func TestValidateAcceptsValidDAGAndTopoOrders(t *testing.T) {
	s := specWith(
		Issue{Ref: "b", Title: "B", Body: "y", Deps: []string{"a"}},
		Issue{Ref: "a", Title: "A", Body: "x"},
	)
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	order, err := s.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	// "a" (index 1) must come before "b" (index 0).
	if order[0] != 1 || order[1] != 0 {
		t.Errorf("order = %v, want [1 0]", order)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/board/ -run TestValidate`
Expected: FAIL — `s.Validate undefined`.

- [ ] **Step 3: Implement `validate.go`**

```go
package board

import (
	"errors"
	"fmt"
)

// Validate returns a single error joining every problem in the spec, or nil.
func (s *Spec) Validate() error {
	var errs []error
	if s.Team == "" {
		errs = append(errs, errors.New("team is required"))
	}
	seen := map[string]bool{}
	for _, is := range s.Issues {
		switch {
		case is.Ref == "":
			errs = append(errs, errors.New("issue with empty ref"))
		case seen[is.Ref]:
			errs = append(errs, fmt.Errorf("duplicate ref %q", is.Ref))
		default:
			seen[is.Ref] = true
		}
		if is.Title == "" {
			errs = append(errs, fmt.Errorf("issue %q: title is required", is.Ref))
		}
		if is.Body == "" {
			errs = append(errs, fmt.Errorf("issue %q: body or body_file is required", is.Ref))
		}
		for _, l := range is.Labels {
			if l == "" {
				errs = append(errs, fmt.Errorf("issue %q: empty label", is.Ref))
			}
		}
	}
	for _, is := range s.Issues {
		for _, d := range is.Deps {
			if !seen[d] {
				errs = append(errs, fmt.Errorf("issue %q: dep %q is not a defined ref", is.Ref, d))
			}
		}
	}
	if _, err := s.TopoOrder(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// TopoOrder returns issue indices in dependency order (a ref's blockers
// precede it). Errors if the dep graph has a cycle. Refs not present are
// treated as satisfied (Validate reports them separately).
func (s *Spec) TopoOrder() ([]int, error) {
	idxByRef := make(map[string]int, len(s.Issues))
	for i, is := range s.Issues {
		idxByRef[is.Ref] = i
	}
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make([]int, len(s.Issues))
	var order []int
	var visit func(i int) error
	visit = func(i int) error {
		switch color[i] {
		case black:
			return nil
		case grey:
			return fmt.Errorf("dependency cycle through ref %q", s.Issues[i].Ref)
		}
		color[i] = grey
		for _, d := range s.Issues[i].Deps {
			if j, ok := idxByRef[d]; ok {
				if err := visit(j); err != nil {
					return err
				}
			}
		}
		color[i] = black
		order = append(order, i)
		return nil
	}
	for i := range s.Issues {
		if err := visit(i); err != nil {
			return nil, err
		}
	}
	return order, nil
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/board/ -run TestValidate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/board/validate.go internal/board/validate_test.go
git commit -m "feat(board): validate spec refs, deps, and acyclicity"
```

### Task 3: Content marker (ref + sha) render/parse

**Files:**
- Create: `internal/board/marker.go`
- Test: `internal/board/marker_test.go`

**Interfaces:**
- Consumes: `Issue` (Task 1).
- Produces: `func ContentSHA(is Issue) string` — 8-hex-char sha256 over title+body+sorted labels+sorted deps; `func RenderMarker(ref, sha string) string` → `"<!-- clipse-ref: <ref> sha:<sha> -->"`; `func ParseMarker(description string) (ref, sha string, ok bool)` — finds the trailer in a Linear description; `func StripMarker(description string) string` — description without the trailer; `func WithBody(is Issue) string` — the full description to send to Linear = body + "\n\n" + marker.

- [ ] **Step 1: Write the failing tests**

```go
package board

import "testing"

func TestMarkerRoundTrip(t *testing.T) {
	is := Issue{Ref: "core-1", Title: "T", Body: "b", Labels: []string{"agent:coder"}}
	sha := ContentSHA(is)
	desc := WithBody(is)
	ref, gotSHA, ok := ParseMarker(desc)
	if !ok || ref != "core-1" || gotSHA != sha {
		t.Fatalf("parse = (%q,%q,%v), want (core-1,%q,true)", ref, gotSHA, ok, sha)
	}
	if StripMarker(desc) != "b" {
		t.Errorf("strip = %q, want b", StripMarker(desc))
	}
}

func TestContentSHAChangesWithContent(t *testing.T) {
	a := Issue{Ref: "x", Title: "T", Body: "b"}
	b := Issue{Ref: "x", Title: "T", Body: "b2"}
	if ContentSHA(a) == ContentSHA(b) {
		t.Error("sha did not change with body")
	}
	// Ref is NOT part of the content hash (identity, not content).
	c := Issue{Ref: "y", Title: "T", Body: "b"}
	if ContentSHA(a) != ContentSHA(c) {
		t.Error("sha changed with ref; ref must not affect content sha")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/board/ -run TestMarker` and `-run TestContentSHA`
Expected: FAIL — undefined functions.

- [ ] **Step 3: Implement `marker.go`**

```go
package board

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var markerRE = regexp.MustCompile(`(?m)\n*<!-- clipse-ref: (\S+) sha:(\S+) -->\s*$`)

// ContentSHA is a short digest over an issue's reconciled content (title,
// body, labels, deps) — NOT its ref. A re-run compares this to the sha stored
// in the on-board marker to decide create/update/skip.
func ContentSHA(is Issue) string {
	labels := append([]string(nil), is.Labels...)
	deps := append([]string(nil), is.Deps...)
	sort.Strings(labels)
	sort.Strings(deps)
	h := sha256.New()
	fmt.Fprintf(h, "title:%s\x00body:%s\x00labels:%s\x00deps:%s",
		is.Title, is.Body, strings.Join(labels, ","), strings.Join(deps, ","))
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}

// RenderMarker formats the hidden trailer stored on a Linear issue.
func RenderMarker(ref, sha string) string {
	return fmt.Sprintf("<!-- clipse-ref: %s sha:%s -->", ref, sha)
}

// WithBody is the full Linear description for an issue: its body followed by
// the marker carrying its ref and current content sha.
func WithBody(is Issue) string {
	return strings.TrimRight(is.Body, "\n") + "\n\n" + RenderMarker(is.Ref, ContentSHA(is))
}

// ParseMarker extracts (ref, sha) from a Linear description's trailer.
func ParseMarker(description string) (string, string, bool) {
	m := markerRE.FindStringSubmatch(description)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// StripMarker returns description without its trailing marker (trimmed).
func StripMarker(description string) string {
	return strings.TrimSpace(markerRE.ReplaceAllString(description, ""))
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/board/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/board/marker.go internal/board/marker_test.go
git commit -m "feat(board): content-sha marker render/parse for idempotency"
```

### Task 4: The planner (pure diff)

**Files:**
- Create: `internal/board/plan.go`
- Test: `internal/board/plan_test.go`

**Interfaces:**
- Consumes: `Spec`, `Issue`, `ContentSHA`, `ParseMarker` (Tasks 1–3).
- Produces:
  `type BoardIssue struct{ ID, Identifier, Description string; BlockedBy []string }` — one existing Linear issue (BlockedBy holds the Linear ids of its blockers);
  `type Action int` with `Create, Update, Skip`;
  `type IssueOp struct{ Ref string; Action Action; Issue Issue; ExistingID string }`;
  `type RelationOp struct{ FromRef, ToRef string }`;
  `type Plan struct{ Issues []IssueOp; Relations []RelationOp; Orphans []string }`;
  `func BuildPlan(spec *Spec, board []BoardIssue) *Plan` — matches each board issue to a spec ref by marker; classifies create/update/skip by sha; every spec dep becomes a RelationOp (the applier is responsible for skipping ones already present); board markers with no spec ref → Orphans.

- [ ] **Step 1: Write the failing tests**

```go
package board

import "testing"

func TestBuildPlanClassifiesCreateUpdateSkipOrphan(t *testing.T) {
	spec := &Spec{Team: "CLI", Issues: []Issue{
		{Ref: "keep", Title: "Keep", Body: "same"},
		{Ref: "chg", Title: "Chg", Body: "new"},
		{Ref: "new", Title: "New", Body: "fresh"},
	}}
	keepSHA := ContentSHA(spec.Issues[0])
	board := []BoardIssue{
		{ID: "L1", Description: "same\n\n" + RenderMarker("keep", keepSHA)},
		{ID: "L2", Description: "old\n\n" + RenderMarker("chg", "deadbeef")},
		{ID: "L9", Description: "x\n\n" + RenderMarker("gone", "cafe")},
	}
	p := BuildPlan(spec, board)
	got := map[string]Action{}
	for _, op := range p.Issues {
		got[op.Ref] = op.Action
	}
	if got["keep"] != Skip || got["chg"] != Update || got["new"] != Create {
		t.Errorf("actions = %v", got)
	}
	if len(p.Orphans) != 1 || p.Orphans[0] != "gone" {
		t.Errorf("orphans = %v, want [gone]", p.Orphans)
	}
	// The update op must carry the existing Linear id.
	for _, op := range p.Issues {
		if op.Ref == "chg" && op.ExistingID != "L2" {
			t.Errorf("chg ExistingID = %q, want L2", op.ExistingID)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/board/ -run TestBuildPlan`
Expected: FAIL — `undefined: BuildPlan`.

- [ ] **Step 3: Implement `plan.go`**

```go
package board

// Action is what BuildPlan decided for one spec issue.
type Action int

const (
	Create Action = iota
	Update
	Skip
)

func (a Action) String() string {
	switch a {
	case Create:
		return "create"
	case Update:
		return "update"
	default:
		return "skip"
	}
}

// BoardIssue is one existing Linear issue as the planner needs to see it.
type BoardIssue struct {
	ID          string
	Identifier  string
	Description string
	BlockedBy   []string // Linear ids of this issue's blockers
}

// IssueOp is the planned action for one spec issue.
type IssueOp struct {
	Ref        string
	Action     Action
	Issue      Issue
	ExistingID string // set for Update/Skip
}

// RelationOp is a blocked-by edge the spec requires.
type RelationOp struct {
	FromRef string // the dependent
	ToRef   string // the blocker
}

// Plan is the full reconciliation plan.
type Plan struct {
	Issues    []IssueOp
	Relations []RelationOp
	Orphans   []string
}

// BuildPlan diffs a validated spec against the current board (matched by
// marker ref) and returns the reconciliation plan. It mutates nothing.
func BuildPlan(spec *Spec, board []BoardIssue) *Plan {
	existingByRef := map[string]BoardIssue{}
	specRefs := map[string]bool{}
	for _, bi := range board {
		if ref, _, ok := ParseMarker(bi.Description); ok {
			existingByRef[ref] = bi
		}
	}
	p := &Plan{}
	for _, is := range spec.Issues {
		specRefs[is.Ref] = true
		op := IssueOp{Ref: is.Ref, Issue: is}
		if bi, ok := existingByRef[is.Ref]; ok {
			op.ExistingID = bi.ID
			_, sha, _ := ParseMarker(bi.Description)
			if sha == ContentSHA(is) {
				op.Action = Skip
			} else {
				op.Action = Update
			}
		} else {
			op.Action = Create
		}
		p.Issues = append(p.Issues, op)
		for _, d := range is.Deps {
			p.Relations = append(p.Relations, RelationOp{FromRef: is.Ref, ToRef: d})
		}
	}
	for ref := range existingByRef {
		if !specRefs[ref] {
			p.Orphans = append(p.Orphans, ref)
		}
	}
	return p
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/board/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/board/plan.go internal/board/plan_test.go
git commit -m "feat(board): diff spec against board into a reconciliation plan"
```

### Task 5: Plan rendering + example spec + schema + drift test

**Files:**
- Create: `internal/board/render.go`
- Test: `internal/board/render_test.go`
- Create: `configs/board.example.yaml`
- Create: `schema/board-spec.schema.json`
- Test: `internal/board/example_test.go`

**Interfaces:**
- Consumes: `Plan`, `IssueOp`, `RelationOp` (Task 4).
- Produces: `func (p *Plan) Render() string` — the human table + summary line shown by `clipse board plan`.

- [ ] **Step 1: Write the failing tests**

```go
package board

import (
	"strings"
	"testing"
)

func TestPlanRenderSummary(t *testing.T) {
	p := &Plan{
		Issues: []IssueOp{
			{Ref: "a", Action: Create, Issue: Issue{Title: "A"}},
			{Ref: "b", Action: Skip, Issue: Issue{Title: "B"}},
		},
		Relations: []RelationOp{{FromRef: "b", ToRef: "a"}},
		Orphans:   []string{"z"},
	}
	out := p.Render()
	if !strings.Contains(out, "+ create") || !strings.Contains(out, "= skip") {
		t.Errorf("missing action rows:\n%s", out)
	}
	if !strings.Contains(out, "1 create") || !strings.Contains(out, "1 orphan") {
		t.Errorf("bad summary:\n%s", out)
	}
}

func TestExampleSpecParsesAndValidates(t *testing.T) {
	spec, err := Parse("../../configs/board.example.yaml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/board/ -run 'TestPlanRender|TestExampleSpec'`
Expected: FAIL — `p.Render undefined`, then missing example file.

- [ ] **Step 3: Implement `render.go`**

```go
package board

import (
	"fmt"
	"strings"
)

// Render formats a Plan as the human-facing plan table plus a summary line.
func (p *Plan) Render() string {
	var b strings.Builder
	var nc, nu, ns int
	for _, op := range p.Issues {
		var sym string
		switch op.Action {
		case Create:
			sym, nc = "+ create", nc+1
		case Update:
			sym, nu = "~ update", nu+1
		case Skip:
			sym, ns = "= skip  ", ns+1
		}
		fmt.Fprintf(&b, "  %s  %-8s %s\n", sym, op.Ref, op.Issue.Title)
	}
	for _, r := range p.Relations {
		fmt.Fprintf(&b, "  + relation %s blocked-by %s\n", r.FromRef, r.ToRef)
	}
	for _, o := range p.Orphans {
		fmt.Fprintf(&b, "  ! orphan  %-8s (on board, not in spec — left alone)\n", o)
	}
	fmt.Fprintf(&b, "\nplan: %d create, %d update, %d skip, %d relation, %d orphan\n",
		nc, nu, ns, len(p.Relations), len(p.Orphans))
	return b.String()
}
```

- [ ] **Step 4: Create `configs/board.example.yaml`**

```yaml
# Example clipse board spec. `clipse board plan board.yaml` previews the
# reconciliation; `clipse board apply board.yaml` executes it. Idempotent:
# re-running only creates/updates what changed (matched by ref marker).
team: CLI
base_branch: main
default_labels: [agent:coder]
issues:
  - ref: core-1
    title: Greeter core
    labels: [agent:coder]
    deps: []
    body: |
      Implement the core greeting function with a unit test.
  - ref: cfg-1
    title: Config dataclass
    deps: [core-1]
    body_file: specs/cfg-1.md
  - ref: docs-1
    title: Write the README usage section
    human: true
    deps: [core-1, cfg-1]
    body: |
      Human-authored: document usage once the CLI stabilizes.
```

Note: `specs/cfg-1.md` beside the example is only needed if a test parses body_file resolution for it; the drift test above uses inline+file. Create `configs/specs/cfg-1.md` with a one-line body so Parse succeeds:
```
Config dataclass with validation and a unit test.
```

- [ ] **Step 5: Create `schema/board-spec.schema.json`** (reference only — documents the shape; not codegen'd, not loaded at runtime)

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "title": "clipse board spec",
  "type": "object",
  "required": ["team", "issues"],
  "properties": {
    "team": { "type": "string", "minLength": 1 },
    "base_branch": { "type": "string" },
    "default_labels": { "type": "array", "items": { "type": "string", "minLength": 1 } },
    "issues": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["ref", "title"],
        "properties": {
          "ref": { "type": "string", "minLength": 1 },
          "title": { "type": "string", "minLength": 1 },
          "milestone": { "type": "string" },
          "labels": { "type": "array", "items": { "type": "string", "minLength": 1 } },
          "deps": { "type": "array", "items": { "type": "string", "minLength": 1 } },
          "human": { "type": "boolean" },
          "body": { "type": "string" },
          "body_file": { "type": "string" }
        },
        "oneOf": [{ "required": ["body"] }, { "required": ["body_file"] }],
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}
```

- [ ] **Step 6: Run to verify pass**

Run: `go test ./internal/board/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/board/render.go internal/board/render_test.go internal/board/example_test.go configs/board.example.yaml configs/specs schema/board-spec.schema.json
git commit -m "feat(board): plan rendering, example spec, and reference schema"
```

---

## Phase 2 — transport extract + read + `clipse board plan`

### Task 6: Extract shared GraphQL transport

**Files:**
- Create: `internal/linear/transport.go`
- Modify: `internal/linear/http_client.go` (replace the `do` method + auth/envelope internals with the shared transport; keep `HTTPClient`'s public methods unchanged)
- Test: existing `internal/linear/http_client_loopback_test.go` must still pass unchanged.

**Interfaces:**
- Produces: `type transport struct{ apiKey, baseURL string; httpClient *http.Client }`; `func newTransport(baseURL string) (*transport, error)` (reads `LINEAR_API_KEY`, 30s timeout); `func (t *transport) do(ctx context.Context, reqBody []byte) ([]byte, error)` (moves the current `do` verbatim); the `graphqlResponse` type moves here. `HTTPClient` embeds `*transport` so `c.do(...)` still resolves.

- [ ] **Step 1: Move transport out**

Create `internal/linear/transport.go` with `transport`, `newTransport`, `do`, and `graphqlResponse` (cut from `http_client.go`). Change `HTTPClient` to embed `*transport` (remove its `apiKey`/`baseURL`/`httpClient` fields; keep `teamKey`/`teamID`/`labelPrefix`/`mu`/`stateIDs`). Update `newHTTPClient` to build the transport:

```go
func newHTTPClient(baseURL, teamKey, teamID, labelPrefix string) (*HTTPClient, error) {
	tr, err := newTransport(baseURL)
	if err != nil {
		return nil, fmt.Errorf("building linear http client: %w", err)
	}
	return &HTTPClient{transport: tr, teamKey: teamKey, teamID: teamID, labelPrefix: labelPrefix}, nil
}
```

- [ ] **Step 2: Run the full linear suite (behavior must be unchanged)**

Run: `go test -race ./internal/linear/...`
Expected: PASS (loopback tests unchanged).

- [ ] **Step 3: Run the whole build/gate**

Run: `go build ./... && go test -race ./...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/linear/transport.go internal/linear/http_client.go
git commit -m "refactor(linear): extract shared graphql transport"
```

### Task 7: Bootstrap client — team, states, labels, issue read

**Files:**
- Create: `internal/linear/bootstrap/client.go`
- Create: `internal/linear/bootstrap/queries.go`
- Test: `internal/linear/bootstrap/client_test.go`

**Interfaces:**
- Consumes: `transport` package internals via a small exported constructor (add `func NewTransport(baseURL string) (*Transport, error)` and export `Transport.Do` in `internal/linear`, OR — preferred to keep the surface tight — give bootstrap its own copy of the ~30-line transport since it is a distinct client). **Decision: bootstrap gets its own unexported transport** (copy of `do`), so `internal/linear`'s API does not widen. Duplicated code is ~30 lines and fully covered by loopback tests on both sides.
- Produces on `type Client struct{…}`:
  `func NewClient(baseURL, teamKey string) (*Client, error)` (reads `LINEAR_API_KEY`);
  `func (c *Client) Resolve(ctx) error` (fetches team id + start-state id + existing label ids; caches);
  `func (c *Client) TeamIssues(ctx) ([]board.BoardIssue, error)` (all non-archived team issues with descriptions + inverseRelations→BlockedBy);
  Use `NewClientWithBaseURL` naming parallel to `internal/linear` for the httptest form; `NewClient` points at the real API URL.

- [ ] **Step 1: Write the failing test (httptest loopback for TeamIssues)**

```go
package bootstrap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTeamIssuesParsesMarkersAndBlockers(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test-key")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"team":{"issues":{"nodes":[
			{"id":"L1","identifier":"CLI-1","description":"b\n\n<!-- clipse-ref: core-1 sha:abc123de -->","inverseRelations":{"nodes":[]}},
			{"id":"L2","identifier":"CLI-2","description":"x","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"L1"}}]}}
		]}}}}`))
	}))
	defer srv.Close()

	c, err := NewClientWithBaseURL(srv.URL, "CLI")
	if err != nil {
		t.Fatalf("NewClientWithBaseURL: %v", err)
	}
	issues, err := c.TeamIssues(context.Background())
	if err != nil {
		t.Fatalf("TeamIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues", len(issues))
	}
	if issues[1].ID != "L2" || len(issues[1].BlockedBy) != 1 || issues[1].BlockedBy[0] != "L1" {
		t.Errorf("L2 blockers = %v", issues[1].BlockedBy)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/linear/bootstrap/`
Expected: FAIL — package/functions undefined.

- [ ] **Step 3: Implement `queries.go` + `client.go`**

`queries.go` holds: a `TeamIssuesQuery` (`team(key…){ issues(first:250){ nodes{ id identifier description inverseRelations{ nodes{ type issue{ id } } } } } }` — note: match by the team's `key`; if Linear's schema requires filtering issues by team key, mirror `CandidateIssuesQuery`'s `issues(filter:{team:{key:{eq:$teamKey}}})` form instead), plus `TeamMetaQuery` (team id + states id/name/type + labels id/name), `IssueCreateMutation`, `IssueUpdateMutation`, `IssueRelationCreateMutation`, `IssueLabelCreateMutation`.

`client.go` holds the unexported transport copy (`do`, auth header `Authorization: <key>`, GraphQL-errors check — copied from `internal/linear/transport.go`), `NewClient`/`NewClientWithBaseURL`, `Resolve`, and `TeamIssues` (decode nodes → `[]board.BoardIssue`; map each `inverseRelations` node of `type=="blocks"` to a `BlockedBy` id).

Full `TeamIssues` decode:
```go
func (c *Client) TeamIssues(ctx context.Context) ([]board.BoardIssue, error) {
	body, err := marshalGraphQL(TeamIssuesQuery, map[string]any{"teamKey": c.teamKey})
	if err != nil {
		return nil, fmt.Errorf("team issues: %w", err)
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("team issues: %w", err)
	}
	var payload struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					ID               string `json:"id"`
					Identifier       string `json:"identifier"`
					Description      string `json:"description"`
					InverseRelations struct {
						Nodes []struct {
							Type  string `json:"type"`
							Issue struct{ ID string `json:"id"` } `json:"issue"`
						} `json:"nodes"`
					} `json:"inverseRelations"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, fmt.Errorf("team issues: decoding: %w", err)
	}
	out := make([]board.BoardIssue, 0, len(payload.Data.Issues.Nodes))
	for _, n := range payload.Data.Issues.Nodes {
		bi := board.BoardIssue{ID: n.ID, Identifier: n.Identifier, Description: n.Description}
		for _, r := range n.InverseRelations.Nodes {
			if r.Type == "blocks" {
				bi.BlockedBy = append(bi.BlockedBy, r.Issue.ID)
			}
		}
		out = append(out, bi)
	}
	return out, nil
}
```

(Use the `issues(filter:{team:{key:{eq:$teamKey}}})` top-level query shape so the JSON path is `data.issues.nodes` as decoded above.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/linear/bootstrap/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/linear/bootstrap/
git commit -m "feat(bootstrap): linear client reading team issues + markers"
```

### Task 8: `clipse board plan` subcommand

**Files:**
- Create: `cli/board.go`
- Modify: `cli/root.go:31-33` (add `root.AddCommand(newBoardCmd())`)
- Test: `cli/board_test.go` (plan against a fake — see note)

**Interfaces:**
- Consumes: `board.Parse/Validate/BuildPlan/Plan.Render` (Phase 1), `bootstrap.Client.TeamIssues` (Task 7), `internal/config` for defaults.
- Produces: `func newBoardCmd() *cobra.Command` with a `plan <spec.yaml>` subcommand. Reads the spec's `team` (falling back to config `team_key`), builds a bootstrap client, fetches issues, prints `plan.Render()` to stdout. `--apply` deferred to Task 10.

- [ ] **Step 1: Write the failing test**

Test the wiring seam by extracting the pure part: a `func planText(spec *board.Spec, issues []board.BoardIssue) string { return board.BuildPlan(spec, issues).Render() }` in `cli/board.go`, asserted directly (network stays out of the unit test):

```go
package cli

import (
	"strings"
	"testing"

	"clipse/internal/board"
)

func TestPlanText(t *testing.T) {
	spec := &board.Spec{Team: "CLI", Issues: []board.Issue{{Ref: "a", Title: "A", Body: "x"}}}
	if !strings.Contains(planText(spec, nil), "+ create") {
		t.Error("expected create row")
	}
}
```

- [ ] **Step 2–5:** run-fail → implement `newBoardCmd` + `planText` + register in root → run-pass → commit `feat(cli): add clipse board plan subcommand`.

Expected fail: `undefined: planText`. Expected pass after implementing. (Module path is `clipse` — confirm with `head -1 go.mod`.)

---

## Phase 3 — `apply`

### Task 9: Applier over a Linear interface (fake-backed)

**Files:**
- Create: `internal/board/apply.go`
- Test: `internal/board/apply_test.go`

**Interfaces:**
- Consumes: `Plan`, `Issue`, `WithBody`, `Spec.TopoOrder`.
- Produces (interface at the consumer, per clipse convention):
```go
type Linear interface {
	EnsureLabels(ctx context.Context, names []string) error
	StartStateID(ctx context.Context) (string, error)
	CreateIssue(ctx context.Context, in CreateInput) (id string, err error)
	UpdateIssue(ctx context.Context, id string, in UpdateInput) error
	AddBlockedBy(ctx context.Context, dependentID, blockerID string) error
}
type CreateInput struct{ Title, Description, StateID string; Labels []string }
type UpdateInput struct{ Title, Description string; Labels []string }
func Apply(ctx context.Context, l Linear, spec *Spec, p *Plan) error
```
`Apply` creates issues in `spec.TopoOrder()` (blockers first), records ref→new-id, updates changed issues, then wires each `RelationOp` **not already present** (the caller passes the board's existing relations via the plan; skip if the dependent already BlockedBy the blocker). Human issues (`is.Human`) get the `human` label and no `agent:*` label; start state applies to all.

- [ ] **Step 1: Write the failing test** — a `fakeLinear` recording calls; assert create order respects deps, marker present in descriptions (via `WithBody`), and relations wired. (Full fake + assertions written in this step.)
- [ ] **Steps 2–5:** run-fail → implement `Apply` → run-pass → commit `feat(board): apply plan via linear interface`.

### Task 10: Bootstrap mutations + `clipse board apply`

**Files:**
- Modify: `internal/linear/bootstrap/client.go` (implement `board.Linear`: `EnsureLabels`, `StartStateID`, `CreateIssue`, `UpdateIssue`, `AddBlockedBy` via the mutations in `queries.go`)
- Modify: `cli/board.go` (add `apply` subcommand + `--apply` on `plan`; on apply, build plan then `board.Apply`)
- Test: `internal/linear/bootstrap/apply_loopback_test.go`

**Interfaces:**
- Consumes: `board.Linear`, `board.CreateInput`, `board.UpdateInput`.
- Produces: `*bootstrap.Client` satisfies `board.Linear`. `StartStateID` resolves the state mapping to the `ready` column via the team-meta query (reuse `canonicalWorkflowName` logic; the start state is the one whose canonical name maps to `ready`/`Todo`).

- [ ] **Step 1: Write the failing loopback test** — httptest server asserting a create issues an `issueCreate` mutation whose `description` contains the marker, and an `AddBlockedBy` issues `issueRelationCreate` with `type:"blocks"`. (Full server + assertions in this step.)
- [ ] **Steps 2–5:** run-fail → implement mutations + wire `apply` → run-pass → **idempotency test**: apply, feed resulting descriptions back as board state, assert `BuildPlan` is all-`Skip` → commit `feat(cli): clipse board apply reconciles the linear board`.

---

## Phase 4 — smoke.sh migration

### Task 11: smoke `seed()` emits board.yaml + calls `clipse board apply`

**Files:**
- Modify: `scripts/smoke/smoke.sh` (`seed()`, `write_manifest`, `smoke_issue_ids`)
- Modify: `scripts/smoke/README.md`

**Interfaces:** none (shell). Behavior-preserving: same tickets, same `[smoke]` titles, same `blocked-by` DAG, same `CLI-N` identifiers captured for `run`/`verify`.

- [ ] **Step 1:** Add a `dag_to_board_yaml()` bash function that renders the existing `manifest.tsv` + `TNN.md` into a `board.yaml` (ref = `smoke-N`, title = column 2, body_file = the TNN.md path, deps = `smoke-<dep>`, label `agent:coder`, team `$TEAM_KEY`).
- [ ] **Step 2:** Replace the `linear issue create … | grep -oE 'CLI-[0-9]+'` loop + relation loop with `clipse board apply "$board_yaml"`; capture created identifiers from `clipse board plan --json` (add a `--json` output to Task 8's plan for machine parsing) or from a post-apply `TeamIssues` marker read.
- [ ] **Step 3:** Run `bash scripts/smoke/smoke.sh --no-run` (seed only) against the smoke team; verify tickets + relations created, re-run seed → all skip (idempotent).
- [ ] **Step 4:** Commit `refactor(smoke): seed via clipse board apply, drop linear-cli scraping`.

Note (`--json`): fold a `--json` flag into Task 8 so both `plan` and this migration can parse machine output. If time-boxed, the migration may instead read markers back via `TeamIssues`. Do not silently leave two seeders — the bash create/relation path must be deleted, not bypassed.

---

## Phase 5 — milestones/projects (DEFERRED)

Out of scope for this plan (locked in brainstorm). When built: honor `Issue.Milestone` by creating a Linear Project + project milestones on the team and linking issues; add `project:`/`milestones:` to the spec + schema + example; same transport, additive mutations in `bootstrap`. Track as a follow-up.

---

## Phase 6 — authoring skill (new, clipse-specific)

### Task 12: `clipse-board-bootstrap` skill

**Files:**
- Create: `skills/clipse-board-bootstrap/SKILL.md` (repo-versioned; top-level `skills/` because `.claude/` is gitignored here — confirm `.gitignore` before choosing the path)

**Interfaces:** none (prose skill).

- [ ] **Step 1:** Write `SKILL.md` with YAML frontmatter (`name`, `description` matching the discovery convention) instructing: take a prose project plan → decompose into small, single-concern, dependency-ordered issues → emit a schema-valid `board.yaml` + `body_file` markdowns following clipse conventions (`agent:coder` lane label, `blocked-by` DAG, `human: true` for manual work), validated against `schema/board-spec.schema.json`; STOP at the spec and instruct the user to review then run `clipse board plan` / `apply`. The skill never calls Linear directly.
- [ ] **Step 2:** Add a short "Board bootstrap" section to `AGENTS.md` (the tool + skill + the `board.yaml`→`plan`/`apply` flow) and a follow-up note that smoke now seeds through it.
- [ ] **Step 3:** Commit `docs: add clipse-board-bootstrap skill and board bootstrap docs`.

---

## Final gate

- [ ] `make test` green (`go test -race ./...` + `cd agent && uv run pytest`).
- [ ] `make lint` green (`go vet`, gofmt, ruff).
- [ ] `go build ./...` produces `clipse board plan|apply`.
- [ ] Open the PR as a draft; `--force-with-lease` on rewrites; never `--no-verify`.

## Self-review notes

- **Spec coverage:** spec §Component 1 → Tasks 1–2, 5; §marker → Task 3; §planner → Task 4; §plan output → Task 5; §transport guardrail → Tasks 6–7; §plan cmd → Task 8; §apply → Tasks 9–10; §smoke migration → Task 11; §skill → Task 12; §milestones → Phase 5 (deferred, matches spec). §testing → tests in every task.
- **Guardrail check:** create/update/relation/label mutations exist only in `internal/linear/bootstrap`; `internal/linear.HTTPClient` unchanged except the behavior-preserving transport extraction. Dispatcher cannot create/delete.
- **Type consistency:** `BoardIssue`/`Plan`/`IssueOp`/`RelationOp`/`Action`/`Linear`/`CreateInput`/`UpdateInput` names are used identically across Tasks 4, 7, 9, 10.
- **Open item to resolve at execution:** confirm module path (`go.mod` line 1) for import strings; confirm Linear's `issues(filter:{team:{key…}})` vs `team(key…){issues}` shape against the existing `CandidateIssuesQuery` (Task 7 uses the filter form to keep the `data.issues.nodes` path).
