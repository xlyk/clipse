package boardspec

import (
	"fmt"
	"strings"
)

// Render formats a Plan as the human-facing plan table plus a summary line,
// the output of `clipse board plan`.
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
		fmt.Fprintf(&b, "  %s  %-10s %s\n", sym, op.Ref, op.Issue.Title)
	}
	for _, r := range p.Relations {
		fmt.Fprintf(&b, "  + relation %s blocked-by %s\n", r.FromRef, r.ToRef)
	}
	for _, r := range p.StaleRelations {
		fmt.Fprintf(&b, "  ! stale relation %s blocked-by %s (on board, not in spec — left alone)\n", r.FromRef, r.ToRef)
	}
	for _, o := range p.Orphans {
		fmt.Fprintf(&b, "  ! orphan  %-10s (on board, not in spec — left alone)\n", o)
	}
	fmt.Fprintf(&b, "\nplan: %d create, %d update, %d skip, %d relation, %d stale relation, %d orphan\n",
		nc, nu, ns, len(p.Relations), len(p.StaleRelations), len(p.Orphans))
	return b.String()
}
