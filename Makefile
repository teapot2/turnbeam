SHELL := /bin/bash

# Version stamped into the binary; falls back to the module's build info.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test vet vulncheck check dist release-notes fmt fmt-check

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

## dist: package the macOS Apple Silicon binary and its checksum into dist/
dist: build
	@set -euo pipefail; \
	if [ "$$(go env GOOS)/$$(go env GOARCH)" != darwin/arm64 ]; then \
		echo 'make dist must run on macOS on Apple Silicon (the Keychain code needs cgo)' >&2; exit 1; \
	fi; \
	name=turnbeam_$(VERSION:v%=%)_darwin_arm64; \
	rm -rf dist && mkdir -p "dist/$$name"; \
	cp bin/turnbeam LICENSE README.md CHANGELOG.md "dist/$$name/"; \
	COPYFILE_DISABLE=1 tar -C dist -czf "dist/$$name.tar.gz" "$$name"; \
	(cd dist && shasum -a 256 "$$name.tar.gz" > checksums.txt); \
	echo "dist/$$name.tar.gz"

## release-notes: print CHANGELOG.md's section for VERSION (empty if missing)
release-notes:
	@awk -v version='$(VERSION:v%=%)' 'index($$0, "## [" version "]") == 1 { found = 1; next } found && /^(## )?\[/ { exit } found { print }' CHANGELOG.md

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
