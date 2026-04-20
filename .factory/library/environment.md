# Environment

Environment variables, external dependencies, and setup notes.

**What belongs here:** Required env vars, external API keys/services, dependency quirks, platform-specific notes.
**What does NOT belong here:** Service ports/commands (use `.factory/services.yaml`).

---

## Server Configuration

- Copy `config.example.yaml` to `config.yaml` and fill in API keys
- `config.yaml` is gitignored — never commit secrets
- Server listens on `:8000` by default
- Auth required for web UI pages (login via `/ui/login`)
- Health check at `/health` (no auth required)

## External Dependencies

- LLM backend API keys configured in `config.yaml`
- OAuth tokens stored in `data/tokens/` directory (gitignored)
- No external databases required — SQLite only

## Pre-existing Issues (Do Not Fix)

- 5 test failures in `internal/web` and 2 in `internal/identity` due to uncommitted/stale test expectations
- These are unrelated to the chatv2 mission

## Go Version

- Go 1.25.8 linux/amd64
