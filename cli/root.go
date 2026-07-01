// Package cli defines the cobra subcommands for the clipse binary.
// Heavy logic lives in internal/ packages; this package wires the CLI layer.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags -X github.com/xlyk/clipse/cli.Version=...
var Version = "0.1.0-dev"

// NewRootCmd builds and returns the root cobra command.
// Separating construction from execution makes it straightforward to test.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "clipse",
		Short: "Autonomous coding-agent orchestrator",
		Long: `Clipse dispatches coding-agent workers against Linear issues,
running them in isolated git worktrees and driving the board through
a deterministic state machine.`,
		// SilenceUsage keeps the usage block out of error output.
		SilenceUsage: true,
	}

	root.Version = Version
	// Override the default version template so --version prints a tidy line.
	root.SetVersionTemplate(fmt.Sprintf("clipse %s\n", Version))

	return root
}
