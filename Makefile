SHELL := /bin/bash

# Version stamped into the binary; falls back to the module's build info.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet vulncheck check fmt fmt-check

## build: compile ./bin/turnbeam with the current version
build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/turnbeam ./cmd/turnbeam

## test: run all offline tests with the race detector
test:
	go test -race ./...

## vet: report suspicious constructs
vet:
	go vet ./...

## vulncheck: scan dependencies and code for known vulnerabilities (network)
vulncheck:
	go tool govulncheck ./...

## check: everything CI runs, except the networked vulnerability scan
check: fmt-check vet test build

fmt:
	@set -o pipefail; \
	git ls-files --cached --others --exclude-standard -z -- '*.go' | \
		xargs -0 go tool goimports -w

fmt-check:
	@set -euo pipefail; \
	unformatted="$$(git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 go tool goimports -l)"; \
	if [ -n "$$unformatted" ]; then \
		printf '%s\n' 'Go formatting or imports need updating:' "$$unformatted" >&2; \
		printf '%s\n' 'Run make fmt and commit the changes.' >&2; \
		exit 1; \
	fi
