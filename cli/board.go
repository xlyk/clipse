package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/internal/boardspec"
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
	cmd.AddCommand(newBoardApplyCmd())
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

// newBoardApplyCmd builds `clipse board apply <spec.yaml>`: reconcile for real.
func newBoardApplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "apply <spec.yaml>",
		Short: "Reconcile a board.yaml onto Linear (creates/updates issues)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBoardApply(cmd, args[0])
		},
	}
}

// loadSpecAndClient parses+validates the spec and builds a bootstrap client
// scoped to its team.
func loadSpecAndClient(specPath string) (*boardspec.Spec, *bootstrap.Client, error) {
	spec, err := boardspec.Parse(specPath)
	if err != nil {
		return nil, nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, nil, fmt.Errorf("invalid board spec: %w", err)
	}
	client, err := bootstrap.NewClient(spec.Team)
	if err != nil {
		return nil, nil, err
	}
	return spec, client, nil
}

// runBoardPlan parses+validates the spec, reads the team's current issues,
// and prints the reconciliation plan. It mutates nothing.
func runBoardPlan(cmd *cobra.Command, specPath string) error {
	spec, client, err := loadSpecAndClient(specPath)
	if err != nil {
		return err
	}
	issues, err := client.TeamIssues(cmd.Context())
	if err != nil {
		return fmt.Errorf("reading team issues: %w", err)
	}
	text, err := planText(spec, issues)
	if err != nil {
		return fmt.Errorf("building plan: %w", err)
	}
	fmt.Fprint(cmd.OutOrStdout(), text)
	return nil
}

// runBoardApply builds the plan, prints it, then executes it against Linear.
func runBoardApply(cmd *cobra.Command, specPath string) error {
	spec, client, err := loadSpecAndClient(specPath)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	issues, err := client.TeamIssues(ctx)
	if err != nil {
		return fmt.Errorf("reading team issues: %w", err)
	}
	plan, err := boardspec.BuildPlan(spec, issues)
	if err != nil {
		return fmt.Errorf("building plan: %w", err)
	}
	fmt.Fprint(cmd.OutOrStdout(), plan.Render())
	if err := boardspec.Apply(ctx, client, spec, plan); err != nil {
		return fmt.Errorf("applying plan: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\napplied.")
	return nil
}

// planText is the pure core of the plan command: build the plan and render
// it. Split out so it can be unit-tested without a network call.
func planText(spec *boardspec.Spec, issues []boardspec.BoardIssue) (string, error) {
	plan, err := boardspec.BuildPlan(spec, issues)
	if err != nil {
		return "", err
	}
	return plan.Render(), nil
}
