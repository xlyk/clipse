VERSION ?= 0.1.0-dev
LDFLAGS  = -X github.com/xlyk/clipse/cli.Version=$(VERSION)
BINARY   = ./bin/clipse

.PHONY: build test test-go lint run

## build: compile the clipse binary into ./bin/clipse
build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/clipse

## test: run all tests (Go; task P0.2 adds test-py)
test: test-go

## test-go: run Go tests
test-go:
	go test ./...

## lint: vet and format check
lint:
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt: the following files need formatting:"; \
	  echo "$$unformatted"; \
	  exit 1; \
	fi

## run: run clipse via go run (no build step needed)
run:
	go run ./cmd/clipse
