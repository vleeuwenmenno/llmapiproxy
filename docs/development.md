# Development Guide

Guide for building, testing, and contributing to LLM API Proxy.

## Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- Git

## Build

```bash
# Build the binary
make build

# Or with Go directly
go build -o llmapiproxy ./cmd/llmapiproxy
```

The Makefile injects the git version via `ldflags`:

```bash
go build -trimpath -ldflags="-X main.version=$(VERSION)" -o llmapiproxy ./cmd/llmapiproxy
```

## Run

```bash
# Run with default config (data/config.yaml)
make run

# Or with Go directly
go run ./cmd/llmapiproxy serve --config config.yaml

# With debug logging
go run ./cmd/llmapiproxy serve --config config.yaml --log-level debug

# With JSON logging
go run ./cmd/llmapiproxy serve --config config.yaml --log-json
```

## Test

```bash
# Run all tests
make test
# or
go test ./...

# Static analysis
make vet
# or
go vet ./...

# Both
make check
```

## Make Targets

| Target              | Description                         |
| ------------------- | ----------------------------------- |
| `make help`         | Show all targets with descriptions  |
| `make build`        | Build binary with version injection |
| `make run`          | Run with `data/config.yaml`         |
| `make test`         | Run all tests                       |
| `make vet`          | Static analysis                     |
| `make check`        | `vet` + `test`                      |
| `make clean`        | Remove built binary                 |
| `make docker-build` | Build Docker image                  |
| `make up`           | Start Docker container              |
| `make down`         | Stop Docker container               |
| `make restart`      | Restart Docker container            |
| `make logs`         | Tail container logs                 |
| `make ps`           | Show running containers             |
| `make shell`        | Shell into container                |

## CLI Flags

| Flag          | Default            | Description                                 |
| ------------- | ------------------ | ------------------------------------------- |
| `--config`    | `data/config.yaml` | Configuration file path                     |
| `--log-level` | `info`             | Log level: `debug`, `info`, `warn`, `error` |
| `--log-json`  | `false`            | Enable structured JSON logging              |
| `--users-db`  | `data/users.db`    | Users database path (for `user` command)    |

## Project Structure

```
cmd/llmapiproxy/           # Application entrypoint
  root.go                  # Root command, CLI flags
  serve.go                 # Server startup, router setup
  user_cmd.go              # User management CLI (add, list, remove, passwd)
  codex_loopback.go        # Codex loopback testing
  docs/                    # Legacy docs (migration in progress)

internal/
  backend/                 # Backend interface + implementations
    backend.go             # Backend interface definition
    openai.go              # OpenAI-compatible backend
    anthropic.go           # Anthropic Messages API backend
    copilot.go             # GitHub Copilot backend (Device Code Flow)
    codex.go               # OpenAI Codex backend (OAuth PKCE)
    registry.go            # Backend registry, routing resolution
    registry_register.go   # Backend registration from config
    roundrobin.go          # Round-robin tracker
    known_models.go        # 100+ model metadata database
    model_cache_store.go   # Disk-persisted model cache
    tokens/                # OAuth token storage

  chat/
    store.go               # Chat session SQLite storage

  circuit/
    circuit.go             # Circuit breaker implementation
    circuit_test.go        # Circuit breaker tests

  config/
    config.go              # YAML config loading, hot-reload, file watching

  identity/
    apply.go               # Identity spoofing middleware
    profile.go             # Built-in and custom profile definitions

  logger/
    logger.go              # Zerolog initialization

  oauth/
    copilot_exchange.go    # GitHub Copilot token exchange
    codex_oauth.go         # Codex OAuth PKCE flow
    codex_device_code.go   # Codex device code flow
    device_code.go         # Generic device code flow
    token_store.go         # Token persistence to disk
    token_discovery.go     # Token auto-discovery

  proxy/
    handler.go             # HTTP handler (chat completions, responses, models)
    anthropic.go           # Anthropic Messages API handler
    middleware.go           # Auth middleware
    test_server_test.go    # Test server

  quota/
    quota.go               # Quota tracking interface
    openrouter.go          # OpenRouter quota
    zai.go                 # Z.ai quota
    registry.go            # Quota registry

  stats/
    stats.go               # In-memory stats collector
    store.go               # SQLite persistent storage

  users/
    store.go               # User store (SQLite)
    session.go             # Session management (cookie-based)
    middleware.go           # Web UI auth middleware
    hash.go                # Password hashing (bcrypt)

  web/
    web.go                 # Web UI server, all routes and handlers
    static/                # CSS/JS assets (HTMX)
    templates/             # HTML templates (Go templates)
```

## Coding Style

- **Language:** Go, standard `gofmt` formatting
- **Imports:** Grouped as stdlib → third-party → internal, separated by blank lines
- **Naming:** PascalCase for exports, camelCase for unexported. `NewXxx()` constructors.
- **HTTP handlers:** Methods on struct types, registered via Chi router
- **Error wrapping:** Use `fmt.Errorf("...: %w", err)` for error chains
- **Logging:** Use zerolog — never `fmt.Println` or `fmt.Printf` for runtime output

```go
import (
    "fmt"

    "github.com/go-chi/chi/v5"
    log "github.com/rs/zerolog/log"

    "github.com/menno/llmapiproxy/internal/config"
)
```

## Logging

Use zerolog for all logging:

```go
log.Debug().Err(err).Str("model", modelID).Msg("description")
log.Info().Str("backend", b.Name()).Msg("starting")
log.Warn().Str("field", value).Msg("warning message")
log.Error().Err(err).Msg("fatal error")
```

Use `log.Ctx(ctx)` for request-scoped loggers with trace IDs.

## Testing

- Standard Go testing (`testing` package) — no external test frameworks
- Tests in same package (`*_test.go` files)
- Run with `go test ./...`
- Test servers use `httptest.NewServer` for backend simulation

## Commit Messages

Use conventional commit prefixes:

- `feat:` — New feature
- `fix:` — Bug fix
- `chore:` — Maintenance, dependencies
- `docs:` — Documentation
- `format:` — Code formatting

Present tense, lowercase after the prefix:

```
feat: add model selection combobox to playground
fix: update backend toggle routes
chore: add initial implementation
```

## Architecture Notes

### Request Flow

```
HTTP → AuthMiddleware → Handler → Registry.ResolveRoute() → Strategy Handler → Backend → Stats.Collector → Response
```

### Model Resolution

Client requests use `backend/model` format (e.g. `openrouter/openai/gpt-5.2`). The proxy strips the prefix and forwards to the correct backend.

### Backend Interface

All providers implement `backend.Backend`:

```go
type Backend interface {
    Name() string
    ChatCompletion(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    ChatCompletionStream(ctx context.Context, req ChatRequest) (<-chan StreamChunk, error)
    ListModels(ctx context.Context) ([]Model, error)
    SupportsModel(modelID string) bool
}
```

### Never Hardcode Model Names

- Always rely on upstream API `/models` endpoint or config definitions
- Each backend implements `ListModels()` for upstream model fetching
- Model lists are cached via each backend's cache mechanism
- `known_models.go` is for UI display metadata and model capability flags only

## Dependencies

| Dependency            | Purpose                  |
| --------------------- | ------------------------ |
| `go-chi/chi/v5`       | HTTP router              |
| `rs/zerolog`          | Structured logging       |
| `spf13/cobra`         | CLI framework            |
| `fsnotify/fsnotify`   | Config file watching     |
| `modernc.org/sqlite`  | SQLite (pure Go, no CGO) |
| `golang.org/x/crypto` | Bcrypt password hashing  |
| `gopkg.in/yaml.v3`    | YAML parsing             |
