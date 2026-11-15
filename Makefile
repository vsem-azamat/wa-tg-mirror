# One entry point for the checks CI runs. golangci-lint and govulncheck are pinned in tools/*/go.mod
# and run through `go tool`, so nothing needs a global install besides Go.
SHELL := bash

LINT := go tool -modfile=tools/lint/go.mod golangci-lint
VULN := go tool -modfile=tools/vuln/go.mod govulncheck

.PHONY: check fmt lint test vuln tidy-check build hooks help

## check: every CI job
check: tidy-check lint test vuln

## hooks: use the versioned git hooks in .githooks/ (run once after cloning)
hooks:
	git config core.hooksPath .githooks

## fmt: apply the Go formatters
fmt:
	$(LINT) fmt

## lint: Go linters and formatting
lint:
	$(LINT) config verify
	$(LINT) run

## test: unit tests with the race detector, in random order
test:
	go test -race -shuffle=on ./...

## vuln: known vulnerabilities in the code and its dependencies (needs network)
vuln:
	$(VULN) ./...

## tidy-check: go.mod and go.sum are tidy
tidy-check:
	go mod tidy -diff
	cd tools/lint && go mod tidy -diff
	cd tools/vuln && go mod tidy -diff

## build: the static binary in bin/
build:
	CGO_ENABLED=0 go build -trimpath -o bin/wa-tg-mirror ./cmd/wa-tg-mirror

## help: list the targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
