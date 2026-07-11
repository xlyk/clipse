package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/internal/board"
	"github.com/xlyk/clipse/internal/linear/bootstrap"
)

// newBoardCmd builds the `clipse board` subcommand group: operator commands
// that reconcile a declarative board.yaml onto a Linear team. Unlike
// dispatch, these are short-lived and human-run; they read LINEAR_API_KEY
// directly, like dispatch, and never run as a worker.
func newBoardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "board",
		Short: "Reconcile a Linear board from a board.yaml spec",
		Long: `board reconciles a declarative board.yaml (issues, blocked-by deps,
labels) onto a Linear team. 'plan' previews the changes; 'apply' executes
them. Re-runs are idempotent, matched by a hidden ref marker written into
each issue's description.`,
	}
	cmd.AddCommand(newBoardPlanCmd())
	return cmd
}

// newBoardPlanCmd builds `clipse board plan <spec.yaml>`: a read-only preview.
func newBoardPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan <spec.yaml>",
		Short: "Preview the reconciliation of a board.yaml against Linear",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBoardPlan(cmd, args[0])
		},
	}
}

// runBoardPlan parses+validates the spec, reads the team's current issues,
// and prints the reconciliation plan. It mutates nothing.
func runBoardPlan(cmd *cobra.Command, specPath string) error {
	spec, err := board.Parse(specPath)
	if err != nil {
		return err
	}
	if err := spec.Validate(); err != nil {
		return fmt.Errorf("invalid board spec: %w", err)
	}
	client, err := bootstrap.NewClient(spec.Team)
	if err != nil {
		return err
	}
	issues, err := client.TeamIssues(cmd.Context())
	if err != nil {
		return fmt.Errorf("reading team issues: %w", err)
	}
	fmt.Fprint(cmd.OutOrStdout(), planText(spec, issues))
	return nil
}

// planText is the pure core of the plan command: build the plan and render
// it. Split out so it can be unit-tested without a network call.
func planText(spec *board.Spec, issues []board.BoardIssue) string {
	return board.BuildPlan(spec, issues).Render()
}
