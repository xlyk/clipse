package board

import (
	"errors"
	"fmt"
)

// Validate returns a single error joining every problem in the spec, or nil.
// It runs before any network call so the operator sees all issues at once.
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
// precede it). It errors if the dep graph has a cycle. Deps referencing an
// unknown ref are skipped here (Validate reports them separately).
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
