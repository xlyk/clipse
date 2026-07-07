package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/dispatcher"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/linear"
	"github.com/xlyk/clipse/internal/spawn"
	"github.com/xlyk/clipse/internal/store"
)

// defaultConfigPath is the --config default, overridable per-invocation via
// the CLIPSE_CONFIG environment variable.
const defaultConfigPath = "configs/clipse.yaml"

// dispatchFlags holds the parsed --config/--board/--worker flags for
// newDispatchCmd's RunE.
type dispatchFlags struct {
	configPath string
	boardDir   string
	workerBin  string
}

// newDispatchCmd builds the `clipse dispatch` subcommand: the composition
// root that wires config, store, the singleton lock, orphan recovery, and
// the Dispatcher's Run loop into a long-running daemon process.
func newDispatchCmd() *cobra.Command {
	flags := &dispatchFlags{}

	cmd := &cobra.Command{
		Use:   "dispatch",
		Short: "Run the clipse dispatch daemon",
		Long: `dispatch runs the clipse dispatcher as a long-running daemon: it polls
Linear, reconciles finished worker runs, promotes ready-eligible issues,
claims and spawns new workers up to configured caps, and drains the outbox
of pending Linear mirror writes, once per poll interval, until interrupted
(SIGINT/SIGTERM).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDispatch(cmd, flags)
		},
	}

	cmd.Flags().StringVar(&flags.configPath, "config", configPathDefault(), "path to clipse.yaml")
	cmd.Flags().StringVar(&flags.boardDir, "board", "", "board state directory (default: config board_dir)")
	cmd.Flags().StringVar(&flags.workerBin, "worker", "", "override worker command with a single binary (default: config worker.command)")

	return cmd
}

// configPathDefault honors CLIPSE_CONFIG when set, falling back to
// defaultConfigPath otherwise.
func configPathDefault() string {
	if v := os.Getenv("CLIPSE_CONFIG"); v != "" {
		return v
	}
	return defaultConfigPath
}

// resolveBoardDir returns the effective board directory: flagValue when the
// operator passed --board explicitly (non-empty), otherwise cfgValue (from
// config's board_dir, itself defaulted by config.Load when the YAML document
// omits it — see config.Config.BoardDir).
func resolveBoardDir(flagValue, cfgValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return cfgValue
}

// resolveWorkerCommand returns the argv the Spawner execs for every worker
// invocation: a single-element override []string{flagValue} when the
// operator passed --worker explicitly (non-empty binary path — handy for
// pointing straight at a prebuilt testworker binary during manual
// smoke-testing), otherwise cfgCommand (config.Worker.Command — see its doc
// comment for the ["uv", "--project", ..., "run", "clipse-worker"] shape
// production configs use).
func resolveWorkerCommand(flagValue string, cfgCommand []string) []string {
	if flagValue != "" {
		return []string{flagValue}
	}
	return cfgCommand
}

// runDispatch is the dispatch command's composition root: it configures
// structured logging, loads config, acquires the machine-global singleton
// lock, opens the store, wires the real Linear client / local spawner / git
// workspacer, and runs the Dispatcher's Run loop until SIGINT/SIGTERM.
//
// Kept thin deliberately: all daemon-lifecycle logic (recover-once, the
// poll loop, graceful shutdown) lives in dispatcher.Run.
func runDispatch(cmd *cobra.Command, flags *dispatchFlags) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load(flags.configPath)
	if err != nil {
		return fmt.Errorf("loading config %s: %w", flags.configPath, err)
	}

	boardDir := resolveBoardDir(flags.boardDir, cfg.BoardDir)
	workerCommand := resolveWorkerCommand(flags.workerBin, cfg.Worker.Command)

	if err := os.MkdirAll(boardDir, 0o755); err != nil {
		return fmt.Errorf("creating board dir %s: %w", boardDir, err)
	}

	dbPath := filepath.Join(boardDir, "clipse.db")
	lockPath := filepath.Join(boardDir, "clipse.lock")
	worktreeRoot := filepath.Join(boardDir, "worktrees")

	release, err := dispatcher.AcquireSingleton(lockPath)
	if err != nil {
		if errors.Is(err, dispatcher.ErrAlreadyRunning) {
			fmt.Fprintf(cmd.ErrOrStderr(), "clipse dispatch: another dispatcher already holds the lock at %s\n", lockPath)
			return err
		}
		return fmt.Errorf("acquiring singleton lock %s: %w", lockPath, err)
	}
	defer func() {
		if err := release(); err != nil {
			logger.Error("releasing singleton lock failed", "error", err)
		}
	}()

	// cfg.CheckpointsDir is where the dispatcher roots each issue's
	// --checkpoint-db (dispatcher.checkpointDBPath); the kernel owns this
	// path, so it ensures the directory exists rather than leaving that to
	// each spawned worker.
	if err := os.MkdirAll(cfg.CheckpointsDir, 0o755); err != nil {
		return fmt.Errorf("creating checkpoints dir %s: %w", cfg.CheckpointsDir, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("opening store %s: %w", dbPath, err)
	}
	defer func() {
		if err := st.Close(); err != nil {
			logger.Error("closing store failed", "error", err)
		}
	}()

	// NewHTTPClient scopes candidate-issue polling and workflow-state
	// resolution (SetState's column -> Linear state-id mapping) to the
	// configured team; see internal/linear/state_resolver.go.
	lc, err := linear.NewHTTPClient(cfg.TeamKey, cfg.TeamID, cfg.LaneLabelPrefix)
	if err != nil {
		return fmt.Errorf("building linear client: %w", err)
	}

	spawner := spawn.NewLocalSpawner(workerCommand, boardDir)
	ws := dispatcher.NewGitWorkspacer(cfg.Repo.Path, cfg.Repo.BaseBranch, worktreeRoot)

	d := dispatcher.New(*cfg, st, lc, spawner, ws, dispatcher.WithLogger(logger))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return d.Run(ctx)
}
