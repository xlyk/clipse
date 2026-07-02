# CI workflow outline — greet

Triggers: every push and every pull-request event (all branches).

## Jobs

All three jobs run on `ubuntu-latest` and execute in parallel.

### `go`

1. Check out the repository (`actions/checkout@v4`).
2. Set up Go 1.25.x (`actions/setup-go@v5`).
3. `go build ./...` — confirm the package compiles cleanly.
4. `go vet ./...` — static analysis.
5. `go test -race ./...` — run the full test suite with the race detector.
6. `gofmt -l .` — fail if any file is not gofmt-clean.

### `python`

1. Check out the repository.
2. Set up `uv` (`astral-sh/setup-uv@v5`).
3. `cd agent && uv sync` — install Python dependencies.
4. `cd agent && uv run pytest` — run the Python test suite.
5. `cd agent && uvx ruff check .` — lint the agent source.

### `codegen-drift`

Catches any gap between the JSON schemas in `schema/` and the generated files
(`internal/contract/contract.go`, `agent/src/clipse_agent/contract.py`).

1. Check out the repository.
2. Set up Go 1.25.x and `uv`.
3. `make codegen` — regenerate both files from `schema/`.
4. `git diff --exit-code` — fail if regeneration produces any diff.

## Notes

- The race detector (`-race`) is non-negotiable; it catches data races that unit assertions alone miss.
- `gofmt` is checked, not applied — formatting must be committed by the author, not silently fixed by CI.
- The codegen-drift job enforces that `schema/` is the single source of truth; hand-editing the generated files will cause this job to fail.
- No network access beyond module proxy (`GONOSUMCHECK`, `GOFLAGS=-mod=readonly` are reasonable additions once a `go.sum` is stable).
