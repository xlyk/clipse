# CI workflow outline — greet

Triggers: every push and every pull-request event (all branches).

## Jobs

### `build`

1. Check out the repository (`actions/checkout@v4`).
2. Set up Go 1.25.x (`actions/setup-go@v5`).
3. `go build ./...` — confirm the package compiles cleanly.

### `test`

Depends on `build`.

1. Check out the repository.
2. Set up Go 1.25.x.
3. `go test -race ./...` — run the full test suite with the race detector.

### `lint`

Runs in parallel with `test`.

1. Check out the repository.
2. Set up Go 1.25.x.
3. `go vet ./...` — static analysis.
4. `gofmt -l .` — fail if any file is not gofmt-clean.

## Notes

- All jobs run on `ubuntu-latest`.
- The race detector (`-race`) is non-negotiable; it catches data races that unit assertions alone miss.
- `gofmt` is checked, not applied — formatting must be committed by the author, not silently fixed by CI.
- No network access beyond module proxy (`GONOSUMCHECK`, `GOFLAGS=-mod=readonly` are reasonable additions once a `go.sum` is stable).
