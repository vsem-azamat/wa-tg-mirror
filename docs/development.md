# Development

- `make check` runs what CI runs: tidy check, golangci-lint (pinned in `tools/lint/go.mod`), tests with the race detector, govulncheck (pinned in `tools/vuln/go.mod`). CI runs it as one job, `Check`, on pull requests, weekly, and inside the deploy workflow before the image is built.
- `main` takes changes only through pull requests with a green `Check`, squash-merged. Dependabot opens weekly grouped updates for actions, Go modules and the base images.
- `make fmt` applies gofumpt and goimports.
- `make hooks` installs the pre-push hook that runs `make check`.
- `make build` writes a static binary to `bin/`.

The bridge logic in `internal/bridge` depends on small interfaces for WhatsApp and the bots, so its tests use fakes and an SQLite file in a temporary directory.
