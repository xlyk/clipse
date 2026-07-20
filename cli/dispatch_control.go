package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/internal/store"
)

const controlPollInterval = 100 * time.Millisecond

func newDispatchPauseCmd() *cobra.Command {
	var boardDir string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "pause",
		Short: "Prevent new dispatcher claims",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, board, err := openExistingControlStore(boardDir)
			if err != nil {
				return err
			}
			defer st.Close()
			requestID, err := newControlRequestID()
			if err != nil {
				return err
			}
			if err := st.RequestPause(cmd.Context(), requestID, time.Now().Unix()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "board: %s\nrequest: %s\ndesired mode: paused\n", board, requestID)
			if !wait {
				return nil
			}
			control, err := waitForControl(cmd.Context(), st, timeout, func(control store.DispatcherControl) (bool, error) {
				if control.RequestID != requestID {
					return false, fmt.Errorf("pause request %s was superseded by %s", requestID, control.RequestID)
				}
				return control.AcknowledgedAt > 0 && control.ObservedMode == store.ObservedPaused, nil
			})
			if err != nil {
				return fmt.Errorf("waiting for dispatcher pause acknowledgment: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "observed mode: %s\nacknowledged at: %d\n", control.ObservedMode, control.AcknowledgedAt)
			return nil
		},
	}
	cmd.Flags().StringVar(&boardDir, "board", "", "existing board state directory (required)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the active dispatcher to acknowledge pause")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Second, "maximum acknowledgment wait")
	_ = cmd.MarkFlagRequired("board")
	return cmd
}

func newDispatchDrainCmd() *cobra.Command {
	var boardDir string
	var wait bool
	var strict bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "drain",
		Short: "Pause claims and let the active dispatcher exit quiescently",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, board, err := openExistingControlStore(boardDir)
			if err != nil {
				return err
			}
			defer st.Close()
			requestID, err := newControlRequestID()
			if err != nil {
				return err
			}
			if err := st.RequestDrain(cmd.Context(), requestID, time.Now().Unix(), strict); err != nil {
				return err
			}
			control, err := st.ReadDispatcherControl(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "board: %s\nrequest: %s\ndesired mode: paused\ndrain target: %s\nstrict: %t\n",
				board, requestID, displayControlValue(control.DrainTargetInstanceID), strict)
			if control.DrainTargetInstanceID == "" {
				return errors.New("no active dispatcher was available to target; board remains paused")
			}
			if !wait {
				return nil
			}
			control, err = waitForControl(cmd.Context(), st, timeout, func(control store.DispatcherControl) (bool, error) {
				if control.RequestID != requestID {
					return false, fmt.Errorf("drain request %s was superseded by %s", requestID, control.RequestID)
				}
				if control.DrainedAt > 0 {
					return true, nil
				}
				if control.DrainTargetInstanceID == "" {
					return false, errors.New("targeted drain was interrupted before completion")
				}
				return false, nil
			})
			if err != nil {
				return fmt.Errorf("waiting for dispatcher drain (board remains paused): %w", err)
			}
			counts, err := st.DispatcherRuntimeCounts(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "drained at: %d\nactive runs: %d\npending outbox: %d\npending cleanup: %d\n",
				control.DrainedAt, counts.ActiveRuns, counts.PendingOutbox, counts.PendingCleanup)
			return nil
		},
	}
	cmd.Flags().StringVar(&boardDir, "board", "", "existing board state directory (required)")
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for the targeted dispatcher to exit quiescently")
	cmd.Flags().DurationVar(&timeout, "timeout", 65*time.Minute, "maximum drain wait")
	cmd.Flags().BoolVar(&strict, "strict", false, "also wait for pending outbox and workspace cleanup")
	_ = cmd.MarkFlagRequired("board")
	return cmd
}

func newDispatchResumeCmd() *cobra.Command {
	var boardDir string
	var cancelDrain bool
	cmd := &cobra.Command{
		Use:   "resume",
		Short: "Allow dispatcher claims",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, board, err := openExistingControlStore(boardDir)
			if err != nil {
				return err
			}
			defer st.Close()
			requestID, err := newControlRequestID()
			if err != nil {
				return err
			}
			if err := st.RequestResume(cmd.Context(), requestID, time.Now().Unix(), cancelDrain); err != nil {
				if errors.Is(err, store.ErrDrainInProgress) {
					return errors.New("a targeted drain is still active; pass --cancel-drain to resume explicitly")
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "board: %s\nrequest: %s\ndesired mode: running\n", board, requestID)
			return nil
		},
	}
	cmd.Flags().StringVar(&boardDir, "board", "", "existing board state directory (required)")
	cmd.Flags().BoolVar(&cancelDrain, "cancel-drain", false, "explicitly cancel an unfinished targeted drain")
	_ = cmd.MarkFlagRequired("board")
	return cmd
}

func newDispatchControlStatusCmd() *cobra.Command {
	var boardDir string
	cmd := &cobra.Command{
		Use:   "control-status",
		Short: "Show dispatcher scheduling and restart safety",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, board, err := openExistingControlStore(boardDir)
			if err != nil {
				return err
			}
			defer st.Close()
			control, err := st.ReadDispatcherControl(cmd.Context())
			if err != nil {
				return err
			}
			counts, err := st.DispatcherRuntimeCounts(cmd.Context())
			if err != nil {
				return err
			}
			return RenderDispatcherControl(cmd.OutOrStdout(), board, control, counts, time.Now())
		},
	}
	cmd.Flags().StringVar(&boardDir, "board", "", "existing board state directory (required)")
	_ = cmd.MarkFlagRequired("board")
	return cmd
}

func openExistingControlStore(boardDir string) (*store.Store, string, error) {
	if strings.TrimSpace(boardDir) == "" {
		return nil, "", errors.New("--board is required")
	}
	board, err := filepath.Abs(boardDir)
	if err != nil {
		return nil, "", fmt.Errorf("resolving board directory %s: %w", boardDir, err)
	}
	dbPath := filepath.Join(board, "clipse.db")
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil, "", fmt.Errorf("no clipse board found at %s", dbPath)
	} else if err != nil {
		return nil, "", fmt.Errorf("checking board db %s: %w", dbPath, err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, "", fmt.Errorf("opening store %s: %w", dbPath, err)
	}
	return st, board, nil
}

func newControlRequestID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating control request id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func waitForControl(ctx context.Context, st *store.Store, timeout time.Duration, done func(store.DispatcherControl) (bool, error)) (store.DispatcherControl, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(controlPollInterval)
	defer ticker.Stop()
	for {
		control, err := st.ReadDispatcherControl(waitCtx)
		if err != nil {
			return store.DispatcherControl{}, err
		}
		complete, err := done(control)
		if err != nil {
			return control, err
		}
		if complete {
			return control, nil
		}
		select {
		case <-waitCtx.Done():
			return control, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

// RenderDispatcherControl formats only durable DB state; it never probes a
// process, so the reported acknowledgment and restart safety cannot race ps.
func RenderDispatcherControl(w io.Writer, board string, control store.DispatcherControl, counts store.DispatcherRuntimeCounts, now time.Time) error {
	safe, reason := dispatcherRestartSafety(control, counts)
	requestAge := "-"
	requestAge = controlRequestAge(control.RequestedAt, now)
	_, err := fmt.Fprintf(w, `board: %s
desired mode: %s
observed mode: %s
active instance: %s
active pid: %s
request: %s
request age: %s
drain target: %s
drain strict: %t
drained at: %s
active runs: %d
pending outbox: %d
pending cleanup: %d
safe to restart: %s (%s)
`, board, control.DesiredMode, control.ObservedMode,
		displayControlValue(control.ActiveInstanceID), displayControlPID(control.ActivePID), displayControlValue(control.RequestID), requestAge,
		displayControlValue(control.DrainTargetInstanceID), control.DrainStrict, displayControlTimestamp(control.DrainedAt),
		counts.ActiveRuns, counts.PendingOutbox, counts.PendingCleanup, yesNo(safe), reason,
	)
	if err != nil {
		return fmt.Errorf("rendering dispatcher control: %w", err)
	}
	return nil
}

func controlRequestAge(requestedAt int64, now time.Time) string {
	if requestedAt <= 0 {
		return "-"
	}
	age := now.Sub(time.Unix(requestedAt, 0)).Round(time.Second)
	if age < 0 {
		age = 0
	}
	return age.String()
}

func dispatcherRestartSafety(control store.DispatcherControl, counts store.DispatcherRuntimeCounts) (bool, string) {
	if control.DesiredMode != store.SchedulingPaused {
		return false, "scheduling is not paused"
	}
	if counts.ActiveRuns != 0 {
		return false, fmt.Sprintf("%d active run(s) remain", counts.ActiveRuns)
	}
	if control.DrainTargetInstanceID != "" && control.DrainedAt == 0 {
		return false, "targeted drain has not completed"
	}
	return true, "scheduling paused and no active runs"
}

func displayControlValue(value string) string {
	if value == "" {
		return "-"
	}
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

func displayControlPID(pid int) string {
	if pid <= 0 {
		return "-"
	}
	return fmt.Sprintf("%d", pid)
}

func displayControlTimestamp(ts int64) string {
	if ts == 0 {
		return "-"
	}
	return fmt.Sprintf("%d", ts)
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
