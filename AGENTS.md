# Repository Guidelines

## Project Structure & Module Organization

This repository is a Go CLI in a single root-level `main` package. `main.go` defines commands; `run.go` coordinates feeds and processing; `article.go`, `fetch.go`, and `digest.go` handle content; `ai.go` and `auth.go` integrate AI; `state.go` tracks progress. Tests live beside source as `*_test.go`, with HTML fixtures in `testdata/`.

`feeds.json` defines RSS sources and modes (`select`, `all`, `digest`). `prompts/` contains Japanese selection and summary instructions. `scripts/run.sh` coordinates Slack delivery and state persistence; `.github/workflows/rss.yml` schedules execution.

## Build, Test, and Development Commands

Use the Go version specified in `go.mod`.

- `go build -o rss-curator .`: build the CLI.
- `go test ./...`: run the offline test suite.
- `go test -run TestName ./...`: run a focused test.
- `go vet ./...`: check for common Go mistakes.
- `staticcheck -checks="all,-ST1000" ./...`: run the documented static analysis checks.
- `./rss-curator run -config feeds.json -state state.json -output /tmp/rss-curator-preview -dry-run`: fetch RSS and list candidates without AI calls, Slack posting, or state updates. The output directory must not already exist.

## Coding Style & Naming Conventions

Format Go changes with `gofmt`; use standard Go naming and tab indentation. Keep files organized by responsibility within the existing package. Follow existing Japanese user-facing messages and documentation conventions. Match the two-space indentation in `feeds.json`. Keep operational shell scripts compatible with Bash and preserve strict error handling.

## Testing Guidelines

Use Go's `testing` package, `TestXxx` names, table-driven cases, `httptest`, and temporary directories. Tests should avoid real external APIs. Cover changed behavior, especially retry boundaries, deduplication, secret redaction, and state persistence after successful delivery. Shell integration tests use mock Slack executables and temporary Git repositories. No numeric coverage minimum is documented.

## Commit & Pull Request Guidelines

Recent commits use `update`; no stricter message convention is evident. Prefer concise, descriptive subjects. PR descriptions should explain the resulting behavior, relevant configuration changes, and validation performed. Link related issues when applicable; screenshots are unnecessary for this CLI.

## Security & Operational Configuration

Keep API keys and Slack tokens in environment variables or Actions Secrets. Never commit secrets, generated summaries, or local state. Normal `scripts/run.sh` execution posts to Slack and pushes the `rss-state` branch; avoid overlapping local and Actions runs. Do not acknowledge failed deliveries or delete state casually, as this affects replay and deduplication.
