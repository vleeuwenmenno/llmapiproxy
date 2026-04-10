# Repository Guidelines

## Project Overview

LLM API Proxy is a Go-based reverse proxy that unifies multiple LLM provider APIs (OpenRouter, Z.ai, OpenCode Zen/Go) behind a single OpenAI-compatible endpoint. It includes a web dashboard, request stats tracking, quota monitoring, and a chat playground.

## Project Structure & Module Organization

```
cmd/llmapiproxy/     # Application entrypoint (main.go)
internal/
  backend/           # Backend interface + OpenAI-compatible implementation + registry
  config/            # YAML config loading, hot-reload (SIGHUP), runtime mutations
  proxy/             # HTTP handler, auth middleware, request routing
  quota/             # Per-provider quota tracking (OpenRouter, Z.ai)
  stats/             # In-memory collector + SQLite persistent store
  web/               # Web UI: dashboard, settings, playground (Go templates + HTMX)
  web/static/        # CSS/JS assets
  web/templates/     # HTML templates
config.example.yaml  # Reference configuration (copy to config.yaml)
```

Module: `github.com/menno/llmapiproxy` (Go 1.25+, uses Chi router, SQLite via modernc.org/sqlite).

## Build, Test, and Development Commands

```bash
make run                # Run the proxy with config.yaml
go build ./cmd/llmapiproxy   # Build binary
go test ./...                # Run all tests
go vet ./...                 # Static analysis
```

The server listens on `:8000` by default. API endpoint: `/v1/chat/completions`. Dashboard: `/ui/`. Health check: `/health`.

## Coding Style & Naming Conventions

- **Language**: Go, standard `gofmt` formatting.
- **Imports**: Grouped as stdlib, then third-party, then internal packages — separated by blank lines.
- **Naming**: PascalCase for exported names, camelCase for unexported. Interface methods follow Go conventions (e.g., `Name()`, `ListModels()`).
- **Constructor pattern**: `NewXxx()` functions return concrete pointer types (e.g., `NewHandler`, `NewRegistry`).
- **HTTP handlers**: Methods on struct types (e.g., `Handler.ChatCompletions`), registered via Chi router.
- **Error wrapping**: Use `fmt.Errorf("...: %w", err)` for error chains.

## Testing Guidelines

- Standard Go testing (`testing` package). No external test frameworks.
- Run with `go test ./...` from the project root.
- Tests should reside in the same package as the code they test (`*_test.go` files).

## Commit & Pull Request Guidelines

- **Commit messages**: Use conventional commit prefixes: `feat:`, `fix:`, `chore:`, `docs:`, `format`.
- Keep messages in present tense, lowercase after the prefix.
- Examples from history: `feat: add model selection combobox to playground`, `fix: update backend toggle routes`, `chore: add initial implementation`.
- **PRs**: Include a description summarizing the change and its motivation.

## Configuration

- Copy `config.example.yaml` to `config.yaml` and fill in API keys.
- `config.yaml` is gitignored — never commit secrets.
- Config is hot-reloadable via `SIGHUP` signal or the web UI settings page.
- Backends declare their type (`openai`), base URL, API key, and optional model lists.

## Architecture Notes

- **Routing**: Client requests use `backend/model` format (e.g., `openrouter/openai/gpt-5.2`). The proxy strips the prefix and forwards to the correct backend.
- **Backend interface**: All providers implement `backend.Backend` (chat completion, streaming, model listing).
- **Stats pipeline**: Requests flow through an in-memory `Collector` with async batching into a SQLite `Store`.
- **Web UI**: Server-rendered Go templates with HTMX for dynamic updates. No frontend build step.
