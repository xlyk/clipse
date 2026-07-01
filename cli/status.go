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

	if _, err := fmt.Fprintln(tw, "STATUS\tCOUNT"); err != nil {
		return fmt.Errorf("writing status header: %w", err)
	}
	for _, status := range sortedStatusKeys(snap.CountsByStatus) {
		if _, err := fmt.Fprintf(tw, "%s\t%d\n", status, snap.CountsByStatus[status]); err != nil {
			return fmt.Errorf("writing status count row: %w", err)
		}
	}

	if _, err := fmt.Fprintln(tw); err != nil {
		return fmt.Errorf("writing section separator: %w", err)
	}

	if _, err := fmt.Fprintln(tw, "IDENTIFIER\tLANE\tSTATUS\tLATEST RUN"); err != nil {
		return fmt.Errorf("writing issue header: %w", err)
	}
	for _, issue := range sortedIssues(snap.Issues) {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			issue.Identifier, issue.LaneLabel, issue.BoardStatus, latestRunCell(issue.LatestRun),
		); err != nil {
			return fmt.Errorf("writing issue row for %s: %w", issue.Identifier, err)
		}
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flushing status table: %w", err)
	}
	return nil
}

// latestRunCell renders a IssueSnapshot.LatestRun as a single table cell:
// "-" when the issue has never had a run, otherwise "<status> turn <n>".
func latestRunCell(run *store.Run) string {
	if run == nil {
		return "-"
	}
	return fmt.Sprintf("%s turn %d", run.Status, run.TurnCount)
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
