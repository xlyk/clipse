// Command testworker is a canned-JSON stand-in for the real Python worker,
// used by internal/spawn and dispatcher tests (Phase 1: zero LLM, zero real
// network — see docs/plans/2026-07-01-clipse-implementation-plan.md). It
// takes the same CLI shape a real worker would (--issue/--lane/--run/
// --thread) plus a --scenario flag selecting canned behavior, and emits a
// schema-valid contract.WorkerResult JSON line to stdout.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/xlyk/clipse/internal/contract"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	var issue, lane, runID, thread, workspace, scenario string
	flag.StringVar(&issue, "issue", "", "Linear issue identifier")
	flag.StringVar(&lane, "lane", "", "worker lane")
	flag.StringVar(&runID, "run", "", "dispatcher-assigned run id")
	flag.StringVar(&thread, "thread", "", "checkpointer thread id")
	flag.StringVar(&workspace, "workspace", "", "worker workspace directory")
	flag.StringVar(&scenario, "scenario", "", "canned scenario to emit")
	flag.Parse()

	// The Spawner builds args strictly from WorkerSpec's fixed fields
	// (--issue/--lane/--run/--thread/--workspace), so tests select the
	// canned scenario via env (WorkerSpec.Env) rather than an extra flag.
	if scenario == "" {
		scenario = os.Getenv("TESTWORKER_SCENARIO")
	}
	if scenario == "" {
		scenario = "done"
	}

	switch scenario {
	case "crash":
		// Simulate a worker that dies before producing any result: partial,
		// non-JSON output on stdout, nonzero exit.
		fmt.Print("panic: something went wrong")
		os.Exit(1)
	case "hang":
		// Simulate a worker that never returns, so the caller's
		// context-deadline kill path is exercised. Sleep far past any
		// realistic test max_runtime; the caller kills us first.
		time.Sleep(10 * time.Minute)
		return nil
	case "malformed":
		// Exit 0 but with output that fails schema-valid JSON parsing.
		fmt.Print("{not json")
		return nil
	}

	outcome, blockKind, err := outcomeFor(scenario)
	if err != nil {
		return err
	}

	result := contract.WorkerResult{
		RunId:     runID,
		IssueId:   issue,
		Lane:      contract.Lane(lane),
		Outcome:   outcome,
		BlockKind: blockKind,
		Summary:   fmt.Sprintf("testworker scenario %q", scenario),
		Artifacts: []string{},
		ThreadId:  thread,
		TurnCount: 1,
		Tokens:    contract.WorkerResultTokens{In: 0, Out: 0},
	}

	enc := json.NewEncoder(os.Stdout)
	if err := enc.Encode(result); err != nil {
		return fmt.Errorf("encoding worker result: %w", err)
	}
	return nil
}

// outcomeFor maps a --scenario value to the (outcome, block_kind) pair to
// emit. block_kind is set iff outcome is "blocked" (schema requirement).
func outcomeFor(scenario string) (contract.WorkerResultOutcome, *contract.BlockKind, error) {
	switch scenario {
	case "done":
		return contract.WorkerResultOutcomeDone, nil, nil
	case "needs_review":
		return contract.WorkerResultOutcomeNeedsReview, nil, nil
	case "changes":
		return contract.WorkerResultOutcomeChangesRequested, nil, nil
	case "continue":
		return contract.WorkerResultOutcomeContinue, nil, nil
	case "blocked":
		bk := contract.BlockKindTransient
		return contract.WorkerResultOutcomeBlocked, &bk, nil
	default:
		return "", nil, fmt.Errorf("testworker: unknown scenario %q", scenario)
	}
}
