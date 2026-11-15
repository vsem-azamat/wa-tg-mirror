# wa-tg-mirror Agent Instructions

This file is the working contract for AI agents and contributors. It says how to work and where to look; facts about the project live in [`docs/`](docs/README.md).

## Setup

Run `make hooks` once after cloning so the versioned git hooks run before every push. Never bypass them with `--no-verify`.

Git worktrees live in `.worktrees/<name>` inside the repository, never next to it.

## Current State Only

- Everything in the repository is written in English.
- Docs describe the system as it is now: no history, plans, roadmaps or status notes. Rationale belongs in the PR description, deferred work in an issue.
- Do not commit scratch files, agent plans, logs or temporary output.

## Public Repository Hygiene

This repository is public. Nothing private goes in: no phone numbers, names of real people or chats, WhatsApp JIDs, Telegram IDs, bot tokens, hostnames, IP addresses, home-directory paths or `.env` contents. The real `config.yaml` and the SQLite database never enter the repository; examples use placeholders.

The bridge handles private messages: message text, captions and file contents never reach logs or error messages. Log chat identifiers only as far as needed to debug, and never tokens.

## Order Of Work

1. Read the relevant docs.
2. Write a failing test for the behavior and watch it fail for the expected reason.
3. Implement the smallest change that makes it pass, then clean up.
4. Update docs when behavior or operation changes.
5. Run `make check` before claiming success or pushing; `make fmt` applies the formatters.

## Quality Bar

- Fix root causes. Do not ship code you cannot explain.
- No abstraction until two real call sites need it. Do not mix unrelated cleanup into a change.
- Fix lint findings instead of silencing them. A `//nolint:<linter>` comment gives its reason on the same line.

## Review And Merge

- Branch from `main`, one topic per branch, conventional commit messages (`feat:`, `fix:`, `docs:`, `chore:`, `test:`, `refactor:`).
- Every change goes branch → pull request → green CI → independent review → squash merge. A merge to `main` deploys.
- Do not add AI attribution lines or trailers to commits or PRs.

## Instruction Files

`AGENTS.md` is the only agent instruction file. Do not add `CLAUDE.md` or tool-specific copies.

## Map

| Path | Contents |
| --- | --- |
| `cmd/wa-tg-mirror/` | The binary: `serve`, `backfill`, `discover`, `check`, `healthcheck`, `version` |
| `internal/config/` | Config file: strict YAML, `${NAME}` tokens from the environment, validation |
| `internal/bridge/` | Routing between WhatsApp and the bots, message conversion both ways, bot profiles |
| `internal/store/` | SQLite tables of the bridge: message links, overflow topics, bot avatars |
| `internal/tg/` | Telegram bots on telego: sending, downloads, long polling, profile |
| `internal/wa/` | WhatsApp on whatsmeow: session store, pairing, names, LIDs, profile pictures |
| `deploy/` | Compose file the deploy workflow ships to the host |
| `docs/` | How the bridge works and how it is operated |
