SHELL := /bin/bash

.PHONY: fmt fmt-check

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
