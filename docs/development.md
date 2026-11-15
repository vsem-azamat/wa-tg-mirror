# Development

- `make check` runs what CI runs: tidy check, golangci-lint (pinned in `tools/lint/go.mod`), tests with the race detector, govulncheck.
- `make fmt` applies gofumpt and goimports.
- `make hooks` installs the pre-push hook that runs `make check`.
- `make build` writes a static binary to `bin/`.

The bridge logic in `internal/bridge` depends on small interfaces for WhatsApp and the bots, so its tests use fakes and an SQLite file in a temporary directory.
