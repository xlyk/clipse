package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"github.com/xlyk/clipse/cli/configureui"
	"github.com/xlyk/clipse/internal/config"
	"github.com/xlyk/clipse/internal/setup"
	setupaudio "github.com/xlyk/clipse/internal/setup/audio"
)

type configureFlags struct {
	output      string
	from        string
	mode        string
	music       string
	noAnimation bool
	noColor     bool
}

func newConfigureCmd() *cobra.Command {
	flags := &configureFlags{}
	cmd := &cobra.Command{
		Use:   "configure",
		Short: "Build and validate a clipse configuration interactively",
		Long: `configure opens a full-screen setup synthesizer for one Clipse instance.
It walks through the repository, Linear state ownership, Daytona backend,
models, safety posture, runtime limits, and isolated state paths; then runs
read-only readiness checks and atomically writes a secret-free YAML file.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConfigure(cmd, flags)
		},
	}
	cmd.Flags().StringVar(&flags.output, "output", "", "config path to write (default configs/clipse.yaml)")
	cmd.Flags().StringVar(&flags.from, "from", "", "existing config to import as editable input")
	cmd.Flags().StringVar(&flags.mode, "mode", "quick", "field visibility: quick or advanced")
	cmd.Flags().StringVar(&flags.music, "music", "auto", "soundtrack mode: auto, on, or off")
	cmd.Flags().BoolVar(&flags.noAnimation, "no-animation", false, "disable visual animation")
	cmd.Flags().BoolVar(&flags.noColor, "no-color", false, "disable ANSI colors")
	return cmd
}

func runConfigure(cmd *cobra.Command, flags *configureFlags) error {
	if flags.mode != "quick" && flags.mode != "advanced" {
		return fmt.Errorf("--mode must be quick or advanced, got %q", flags.mode)
	}
	if flags.music != "auto" && flags.music != "on" && flags.music != "off" {
		return fmt.Errorf("--music must be auto, on, or off, got %q", flags.music)
	}
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("clipse configure requires an interactive terminal; use configs/clipse.example.yaml for non-interactive setup")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolving current directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolving home directory: %w", err)
	}
	output := flags.output
	if output == "" {
		if flags.from != "" {
			output = flags.from
		} else {
			output = filepath.Join(cwd, "configs", "clipse.yaml")
		}
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolving output path: %w", err)
	}

	instance := instanceName(output)
	var draft setup.Draft
	var original []byte
	if flags.from != "" {
		cfg, err := config.Load(flags.from)
		if err != nil {
			return fmt.Errorf("importing config %s: %w", flags.from, err)
		}
		draft = setup.Draft{Instance: instance, Config: *cfg}
		original, err = os.ReadFile(flags.from)
		if err != nil {
			return fmt.Errorf("reading imported config %s: %w", flags.from, err)
		}
	} else {
		draft = setup.NewDraft(instance, cwd, setup.DefaultStateRoot(home))
	}

	player := setupaudio.New(setupaudio.Options{})
	defer player.Stop()
	model := configureui.NewModel(configureui.Options{
		Draft:       draft,
		OutputPath:  output,
		Advanced:    flags.mode == "advanced",
		NoColor:     flags.noColor,
		NoAnimation: flags.noAnimation,
		ASCII:       os.Getenv("TERM") == "dumb",
		Music:       flags.music,
		Context:     cmd.Context(),
		Audio:       player,
		Original:    original,
	})
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithInput(cmd.InOrStdin()), tea.WithOutput(cmd.OutOrStdout()))
	final, err := program.Run()
	if err != nil {
		return fmt.Errorf("running configuration wizard: %w", err)
	}
	finalModel, ok := final.(configureui.Model)
	if !ok {
		return errors.New("configuration wizard returned an unexpected model")
	}
	result := finalModel.Result()
	if result.Canceled || result.WrittenPath == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Clipse configuration canceled; no config was written.")
		return nil
	}
	if result.Err != nil {
		return result.Err
	}

	board := result.BoardDir
	quotedConfig := strconv.Quote(result.WrittenPath)
	quotedBoard := strconv.Quote(board)
	fmt.Fprintf(cmd.OutOrStdout(), "Clipse configuration written to %s\n", result.WrittenPath)
	fmt.Fprintf(cmd.OutOrStdout(), "Readiness: %s\n\n", result.Report.Outcome)
	fmt.Fprintf(cmd.OutOrStdout(), "Launch with a process-scoped LINEAR_API_KEY and the selected provider credentials:\n")
	fmt.Fprintf(cmd.OutOrStdout(), "  ./bin/clipse dispatch --config %s\n", quotedConfig)
	fmt.Fprintf(cmd.OutOrStdout(), "  ./bin/clipse status --board %s\n", quotedBoard)
	fmt.Fprintf(cmd.OutOrStdout(), "  ./bin/clipse tui --board %s\n", quotedBoard)
	if result.BackupPath != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "Previous config backed up to %s\n", result.BackupPath)
	}
	return nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func instanceName(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	name = strings.TrimPrefix(name, "clipse.")
	name = strings.TrimSuffix(name, ".local")
	if name == "clipse" || name == "" {
		return "default"
	}
	return name
}
