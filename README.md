# clipse

Clipse is a personal, autonomous coding-agent orchestrator: a deterministic Go dispatcher watches a Linear board and, for each eligible issue, spins up a headless Python worker in an isolated git worktree; the worker writes code, commits, and opens a PR; downstream lanes review, merge, and document it — all without a human in the inner loop.

See the full technical design at [docs/design/2026-07-01-clipse-design.md](docs/design/2026-07-01-clipse-design.md).
