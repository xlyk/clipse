package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/cli/tui"
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
		// Liveness is I/O (reading the lockfile), so it lives here in the
		// refresh command rather than in the pure Update.
		return tui.SnapshotMsg{Snap: snap, Live: dispatcherLive(lockPath)}
	}

	model := tui.NewModel(tui.WithRefreshCmd(refresh))

	// WithAltScreen takes over the whole terminal (alternate screen buffer):
	// the dashboard renders fullscreen, never bleeds into scrollback, and the
	// terminal is restored on quit. It also fully repaints each frame, which
	// eliminates the inline renderer's line-diff residue (stale cells from a
	// taller/wider previous frame — e.g. the kanban columns — stranded behind
	// a shorter one). WithMouseCellMotion lets the wheel scroll the viewports.
	program := tea.NewProgram(programModel{model}, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running tui: %w", err)
	}
	return nil
}

// dispatcherLive reports whether a clipse dispatcher is currently running,
// the authoritative "the pipeline is live" signal. It reads the PID the
// dispatcher records in its singleton lockfile (see
// dispatcher.AcquireSingleton) and probes that process with signal 0. Reading
// is passive — unlike acquiring the flock, it cannot race a starting
// dispatcher into an ErrAlreadyRunning failure. A missing/empty/garbled
// lockfile, or a PID that no longer exists, all read as not-live.
func dispatcherLive(lockPath string) bool {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	// signal 0 delivers nothing but still resolves the target: nil ⇒ the
	// process is alive; EPERM ⇒ it exists but is owned by another user (still
	// alive); ESRCH ⇒ it is gone.
	err = syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
