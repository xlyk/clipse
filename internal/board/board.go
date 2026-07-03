// Package board implements the pure state-machine transitions and
// dependency-promotion rule described in the design doc's "Board & state
// machine" section. This package is deterministic and side-effect free: no
// I/O, no store/linear/spawn imports, no package-level mutable state. The
// dispatcher is the sole caller and the sole writer of board state; this
// package only computes what the next state *should* be.
package board

import (
	"errors"
	"fmt"

	"github.com/xlyk/clipse/internal/contract"
)

// ErrIllegalTransition is the sentinel wrapped by every error Next returns.
// Callers can check for it with errors.Is.
var ErrIllegalTransition = errors.New("illegal board transition")

// Action tags name what the dispatcher must do to enact a transition, on top
// of moving the card. Next never performs the action itself — it only names
// it — so the dispatcher (or a future outbox) decides how to execute it
// (Linear mirror write, PR comment, respawn, etc).
const (
	// ActionOpenReview means the Coder lane finished a turn with a PR ready
	// for review; the dispatcher opens/updates the Review card.
	ActionOpenReview = "open_review"

	// ActionRequestChanges means the Reviewer lane asked for changes; the
	// dispatcher moves the card to Rework so the Coder lane re-runs.
	ActionRequestChanges = "request_changes"

	// ActionMerge means the Reviewer lane passed; the dispatcher hands the
	// card to the Git-operator lane to auto-merge when CI/branch protection
	// allow it.
	ActionMerge = "merge"

	// ActionDocument means the Git-operator lane landed the PR; the
	// dispatcher hands the card to the always-on Scribe lane.
	ActionDocument = "document"

	// ActionComplete means the Scribe lane finished (wrote docs or no-op'd);
	// the card reaches its terminal Done state.
	ActionComplete = "complete"

	// ActionCommentBlock means a worker reported blocked from an active
	// column; the dispatcher parks the card in Blocked with a reason
	// comment. A human must requeue it (see design doc decision H).
	ActionCommentBlock = "comment_block"

	// ActionRespawn means a worker reported continue; the dispatcher
	// re-spawns another turn in the same column/worktree. Next does not
	// enforce the per-issue turn cap — that is the dispatcher's job (see
	// design doc C2 and the dispatch-loop step 8 note).
	ActionRespawn = "respawn"
)

// transitionKey identifies one (outcome, current column) pair.
type transitionKey struct {
	outcome string
	current string
}

// transitionResult is what a legal transition produces: the target column
// and the action tag naming what the dispatcher must do to enact it.
type transitionResult struct {
	next   string
	action string
}

// transitions is the board's state-machine table, transcribed from the
// design doc's "Board & state machine" table plus the per-lane outcome
// semantics: Running/Rework are Coder-lane columns (needs_review/blocked/
// continue), Review is the Reviewer lane (done=pass/changes_requested/
// blocked), Merging is the Git-operator lane (done=merged/blocked), and
// Documentation is the Scribe lane (done/blocked). Every (outcome, column)
// pair from the contract enums not present here is an illegal transition —
// see TestNext_AllPairsCovered in board_test.go, which audits this table
// against the full cross product.
var transitions = map[transitionKey]transitionResult{
	// Running (Coder lane).
	{outcome: string(contract.WorkerResultOutcomeNeedsReview), current: string(contract.ColumnRunning)}: {
		next: string(contract.ColumnReview), action: ActionOpenReview,
	},
	{outcome: string(contract.WorkerResultOutcomeBlocked), current: string(contract.ColumnRunning)}: {
		next: string(contract.ColumnBlocked), action: ActionCommentBlock,
	},
	{outcome: string(contract.WorkerResultOutcomeContinue), current: string(contract.ColumnRunning)}: {
		next: string(contract.ColumnRunning), action: ActionRespawn,
	},

	// Review (Reviewer lane). "done" here means "pass".
	{outcome: string(contract.WorkerResultOutcomeDone), current: string(contract.ColumnReview)}: {
		next: string(contract.ColumnMerging), action: ActionMerge,
	},
	{outcome: string(contract.WorkerResultOutcomeChangesRequested), current: string(contract.ColumnReview)}: {
		next: string(contract.ColumnRework), action: ActionRequestChanges,
	},
	{outcome: string(contract.WorkerResultOutcomeBlocked), current: string(contract.ColumnReview)}: {
		next: string(contract.ColumnBlocked), action: ActionCommentBlock,
	},

	// Rework (Coder lane re-run; same outcome shape as Running).
	{outcome: string(contract.WorkerResultOutcomeNeedsReview), current: string(contract.ColumnRework)}: {
		next: string(contract.ColumnReview), action: ActionOpenReview,
	},
	{outcome: string(contract.WorkerResultOutcomeBlocked), current: string(contract.ColumnRework)}: {
		next: string(contract.ColumnBlocked), action: ActionCommentBlock,
	},
	{outcome: string(contract.WorkerResultOutcomeContinue), current: string(contract.ColumnRework)}: {
		next: string(contract.ColumnRework), action: ActionRespawn,
	},

	// Merging (Git-operator lane; deterministic executor, board semantics
	// only — see decision log J amendment).
	{outcome: string(contract.WorkerResultOutcomeDone), current: string(contract.ColumnMerging)}: {
		next: string(contract.ColumnDocumentation), action: ActionDocument,
	},
	{outcome: string(contract.WorkerResultOutcomeBlocked), current: string(contract.ColumnMerging)}: {
		next: string(contract.ColumnBlocked), action: ActionCommentBlock,
	},
	// Merging -> Rework: internal/gitops's stale-base-conflict route (a
	// base update landed a real, unresolvable conflict) maps to
	// changes_requested from merging, the same action tag (and cap-checked
	// rework_count bump) as the Reviewer lane's own changes_requested from
	// review -- both mean "the Coder lane gets another attempt".
	{outcome: string(contract.WorkerResultOutcomeChangesRequested), current: string(contract.ColumnMerging)}: {
		next: string(contract.ColumnRework), action: ActionRequestChanges,
	},

	// Documentation (Scribe lane).
	{outcome: string(contract.WorkerResultOutcomeDone), current: string(contract.ColumnDocumentation)}: {
		next: string(contract.ColumnDone), action: ActionComplete,
	},
	{outcome: string(contract.WorkerResultOutcomeBlocked), current: string(contract.ColumnDocumentation)}: {
		next: string(contract.ColumnBlocked), action: ActionCommentBlock,
	},
}

// Next computes the target column and action for a worker-result outcome
// observed while a card sits in current. It returns a non-nil error
// (wrapping ErrIllegalTransition) for any (outcome, current) pair not in the
// design doc's transition table — including Todo/Ready/Done/Blocked, none of
// which have an outcome-driven exit: Todo promotes via Promote (dependency
// state, not a worker outcome), Ready leaves only via the dispatcher's claim
// CAS, Blocked leaves only via a human requeue, and Done is terminal.
func Next(outcome, current string) (next string, action string, err error) {
	result, ok := transitions[transitionKey{outcome: outcome, current: current}]
	if !ok {
		return "", "", fmt.Errorf("%w: outcome %q from column %q", ErrIllegalTransition, outcome, current)
	}
	return result.next, result.action, nil
}

// DepState is the minimal, pure view of a dependency Promote needs: whether
// that dependency has reached a terminal state (done or cancelled). It
// carries no identity or store reference on purpose — internal/board must
// not import the store package. Callers (the dispatcher) are responsible
// for mapping their own dependency records into DepState.
type DepState struct {
	// Terminal is true iff the dependency is done or cancelled and will
	// never re-enter an active column.
	Terminal bool
}

// Promote reports whether a Todo card's dependencies are all clear, i.e.
// whether the dispatcher should move it to Ready. It is true iff current is
// "todo" and every dep is Terminal (an empty deps slice vacuously satisfies
// "every dep", so a dependency-free Todo card promotes immediately). Any
// current column other than "todo" never promotes, regardless of deps: this
// mirrors the design doc's table, where "deps terminal -> Ready" is the only
// listed exit from Todo.
func Promote(current string, deps []DepState) bool {
	if current != string(contract.ColumnTodo) {
		return false
	}
	for _, dep := range deps {
		if !dep.Terminal {
			return false
		}
	}
	return true
}
