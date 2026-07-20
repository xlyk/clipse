package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/internal/store"
)

// newStatusCmd builds the `clipse status` subcommand: a one-shot read of the
// kernel SQLite snapshot rendered as a table. Unlike dispatch, this is a
// short-lived, read-only command, so it opens the store, renders, and closes
// within a single RunE rather than running a daemon loop.
func newStatusCmd() *cobra.Command {
	var boardDir string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the current board state",
		Long: `status reads the kernel's SQLite snapshot (issues and their latest
runs) and renders it as a table: per-status counts followed by one row per
issue.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, boardDir)
		},
	}

	cmd.Flags().StringVar(&boardDir, "board", "./.clipse", "board state directory")

	return cmd
}

// runStatus is the status command's composition root: it opens the store
// read-only at <board>/clipse.db, reads the snapshot, renders it to stdout,
// and closes the store.
//
// Missing-DB behavior: if <board>/clipse.db does not exist, runStatus
// returns a friendly error naming the path rather than treating it as an
// empty board. An empty/never-dispatched board is a meaningfully different
// state from "you pointed --board at the wrong place", and silently
// rendering an empty table would hide that mistake.
func runStatus(cmd *cobra.Command, boardDir string) error {
	dbPath := filepath.Join(boardDir, "clipse.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no clipse board found at %s", dbPath)
	} else if err != nil {
		return fmt.Errorf("checking board db %s: %w", dbPath, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store %s: %w", dbPath, err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "closing store: %v\n", cerr)
		}
	}()

	snap, err := st.ReadSnapshot(context.Background())
	if err != nil {
		return fmt.Errorf("reading snapshot: %w", err)
	}

	if err := RenderStatus(cmd.OutOrStdout(), snap); err != nil {
		return fmt.Errorf("rendering status: %w", err)
	}

	return nil
}

// RenderStatus writes a human-readable table for snap to w: a per-status
// summary line followed by one row per issue (identifier, lane, status,
// latest run). It is pure: no store/DB access, just formatting, so it can be
// tested against a hand-seeded Snapshot without depending on the render's
// wiring to the CLI.
func RenderStatus(w io.Writer, snap store.Snapshot) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if snap.DispatcherControl.DesiredMode != "" {
		safe, reason := dispatcherRestartSafety(snap.DispatcherControl, snap.RuntimeCounts)
		if _, err := fmt.Fprintln(tw, "DISPATCHER\tVALUE"); err != nil {
			return fmt.Errorf("writing dispatcher header: %w", err)
		}
		controlRows := [][2]string{
			{"desired / observed", fmt.Sprintf("%s / %s", snap.DispatcherControl.DesiredMode, snap.DispatcherControl.ObservedMode)},
			{"active instance / pid", fmt.Sprintf("%s / %s", displayControlValue(snap.DispatcherControl.ActiveInstanceID), displayControlPID(snap.DispatcherControl.ActivePID))},
			{"request / age", fmt.Sprintf("%s / %s", displayControlValue(snap.DispatcherControl.RequestID), controlRequestAge(snap.DispatcherControl.RequestedAt, time.Now()))},
			{"drain target", displayControlValue(snap.DispatcherControl.DrainTargetInstanceID)},
			{"active runs", fmt.Sprintf("%d", snap.RuntimeCounts.ActiveRuns)},
			{"pending outbox", fmt.Sprintf("%d", snap.RuntimeCounts.PendingOutbox)},
			{"pending cleanup", fmt.Sprintf("%d", snap.RuntimeCounts.PendingCleanup)},
			{"safe to restart", fmt.Sprintf("%s (%s)", yesNo(safe), reason)},
		}
		for _, row := range controlRows {
			if _, err := fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1]); err != nil {
				return fmt.Errorf("writing dispatcher row %s: %w", row[0], err)
			}
		}
		if _, err := fmt.Fprintln(tw); err != nil {
			return fmt.Errorf("writing dispatcher separator: %w", err)
		}
	}

	if _, err := fmt.Fprintln(tw, "STATUS\tCOUNT"); err != nil {
		return fmt.Errorf("writing status header: %w", err)
	}
	for _, status := range sortedStatusKeys(snap.CountsByStatus) {
		if _, err := fmt.Fprintf(tw, "%s\t%d\n", status, snap.CountsByStatus[status]); err != nil {
			return fmt.Errorf("writing status count row: %w", err)
		}
	}

	if snap.UnmirroredCount > 0 {
		if _, err := fmt.Fprintf(tw, "unmirrored (linear writes pending): %d\n", snap.UnmirroredCount); err != nil {
			return fmt.Errorf("writing unmirrored summary: %w", err)
		}
	}

	if _, err := fmt.Fprintln(tw); err != nil {
		return fmt.Errorf("writing section separator: %w", err)
	}

	if _, err := fmt.Fprintln(tw, "IDENTIFIER\tLANE\tSTATUS\tLATEST RUN\tMIRROR\tBACKEND\tROLE\tSTATE\tSANDBOX"); err != nil {
		return fmt.Errorf("writing issue header: %w", err)
	}
	for _, issue := range sortedIssues(snap.Issues) {
		provider, role, state, sandbox := workspaceCells(issue.Workspace)
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			issue.Identifier, issue.LaneLabel, issue.BoardStatus, latestRunCell(issue.LatestRun), mirrorCell(issue.Unmirrored),
			provider, role, state, sandbox,
		); err != nil {
			return fmt.Errorf("writing issue row for %s: %w", issue.Identifier, err)
		}
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flushing status table: %w", err)
	}
	return nil
}

func workspaceCells(workspace *store.AgentWorkspace) (provider, role, state, sandbox string) {
	if workspace == nil {
		return "-", "-", "-", "-"
	}
	sandbox = workspace.ExternalID
	if sandbox == "" {
		sandbox = "-"
	} else if len(sandbox) > 8 {
		sandbox = sandbox[:8]
	}
	return workspace.Provider, workspace.Role, string(workspace.State), sandbox
}

// latestRunCell renders a IssueSnapshot.LatestRun as a single table cell:
// "-" when the issue has never had a run, otherwise "<status> turn <n>".
func latestRunCell(run *store.Run) string {
	if run == nil {
		return "-"
	}
	return fmt.Sprintf("%s turn %d", run.Status, run.TurnCount)
}

// mirrorCell renders an IssueSnapshot's Unmirrored flag as a single table
// cell: "pending" when the issue has a Linear mirror write still queued in
// the outbox (A2), "-" otherwise.
func mirrorCell(unmirrored bool) string {
	if unmirrored {
		return "pending"
	}
	return "-"
}

// sortedStatusKeys returns counts' keys in a deterministic (alphabetical)
// order, so RenderStatus's summary section doesn't depend on Go's
// unspecified map iteration order.
func sortedStatusKeys(counts map[string]int) []string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedIssues returns issues ordered by Identifier, so the per-issue table
// renders deterministically regardless of the snapshot's underlying id
// ordering.
func sortedIssues(issues []store.IssueSnapshot) []store.IssueSnapshot {
	sorted := make([]store.IssueSnapshot, len(issues))
	copy(sorted, issues)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Identifier < sorted[j].Identifier
	})
	return sorted
}
