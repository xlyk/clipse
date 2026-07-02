package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/cli/tui"
	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/store"
)

// newTUICmd builds the `clipse tui` subcommand: a long-running, interactive
// bubbletea dashboard over the kernel's SQLite snapshot. Unlike `status`
// (one-shot), this opens the store and keeps it open for the life of the
// program, polling on a timer until the user quits.
func newTUICmd() *cobra.Command {
	var boardDir string

	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Run the live board dashboard",
		Long: `tui opens a live, terminal dashboard over the kernel's SQLite
snapshot: issues grouped into RUNNING / BLOCKED / QUEUED, with per-issue
latest-run info and running token totals. It polls the store on a timer and
redraws in place until you press q or ctrl+c.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTUI(cmd, boardDir)
		},
	}

	cmd.Flags().StringVar(&boardDir, "board", "./.clipse", "board state directory")

	return cmd
}

// runTUI is the tui command's composition root: it opens the store at
// <board>/clipse.db, builds the Model with a refresh command that reads a
// fresh snapshot from that store, and runs the bubbletea program until the
// user quits.
//
// Missing-DB behavior mirrors status: a friendly error naming the path
// rather than silently rendering an empty dashboard.
func runTUI(cmd *cobra.Command, boardDir string) error {
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

	lockPath := filepath.Join(boardDir, "clipse.lock")
	refresh := func() tea.Msg {
		snap, err := st.ReadSnapshot(context.Background())
		if err != nil {
			return tui.ErrMsg{Err: fmt.Errorf("reading snapshot: %w", err)}
		}
		// Liveness is I/O (a lock probe), so it lives here in the refresh
		// command rather than in the pure Update.
		return tui.SnapshotMsg{Snap: snap, Live: dispatcherLive(lockPath)}
	}

	model := tui.NewModel(tui.WithRefreshCmd(refresh))

	program := tea.NewProgram(programModel{model})
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running tui: %w", err)
	}
	return nil
}

// dispatcherLive reports whether a clipse dispatcher is currently holding the
// board's singleton lock, which is the authoritative "the pipeline is running"
// signal. It probes non-destructively by trying to acquire the same flock the
// dispatcher takes: if the lock is already held, acquisition fails with
// ErrAlreadyRunning (⇒ live); if it succeeds, no dispatcher is running and the
// probe immediately releases so the TUI never becomes the singleton itself and
// blocks a real dispatcher from starting. Any other error is treated as "can't
// tell" (not live).
func dispatcherLive(lockPath string) bool {
	release, err := dispatcher.AcquireSingleton(lockPath)
	if errors.Is(err, dispatcher.ErrAlreadyRunning) {
		return true
	}
	if err != nil {
		return false
	}
	_ = release()
	return false
}

// programModel adapts tui.Model to the tea.Model interface. tea.Model.Update
// must return the interface type (tea.Model, tea.Cmd), but tui.Model.Update
// deliberately returns the concrete (tui.Model, tea.Cmd) so tests can call
// it and inspect the result's fields/methods directly without a type
// assertion. This adapter is the one place that bridges the two.
type programModel struct {
	tui.Model
}

func (p programModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := p.Model.Update(msg)
	return programModel{next}, cmd
}
