# Usage:
#   make build                  compile ./hooktape
#   make test                   run all tests with the race detector
#   make lint                   check formatting and run go vet
#   make run ARGS="up --no-tunnel"   run from source, passing ARGS to hooktape

# Values stamped into the binary; `hooktape version` prints them.
# They fall back to dev/none when there is no tag or commit to read.
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)

# Module path from go.mod, so it is only written down in one place.
MODULE  := $(shell go list -m 2>/dev/null)

# -X overwrites a string variable at link time. These must match the
# `version` and `commit` vars in internal/cli/version.go.
LDFLAGS := -X $(MODULE)/internal/cli.version=$(VERSION) -X $(MODULE)/internal/cli.commit=$(COMMIT)

# These targets are commands, not files. Without this line, a file or
# directory named "test" would make `make test` say "up to date".
.PHONY: build test lint run

# CGO_ENABLED=0 is set here only: the plan requires pure Go builds,
# but `go test -race` below needs cgo, so it can't be set globally.
build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o hooktape ./cmd/hooktape

test:
	go test -race ./...

# gofmt -l lists unformatted files but still exits 0, so the output is
# checked by hand to make lint actually fail on them.
lint:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi
	go vet ./...

run:
	go run ./cmd/hooktape $(ARGS)
