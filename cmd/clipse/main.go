// Command clipse is the single binary entrypoint for the clipse orchestrator.
// All command wiring and flag definitions live in the cli package.
package main

import (
	"os"

	"github.com/xlyk/clipse/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}
