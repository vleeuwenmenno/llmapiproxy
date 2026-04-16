# LLM API Proxy

> All your LLM providers in one central place.

A lightweight, self-hosted reverse proxy that aggregates multiple LLM provider APIs behind a single OpenAI-compatible endpoint. Keep one API key in your tools, track token usage across all providers, and manage everything from a built-in web dashboard.

---

## Why?

If you have subscriptions at several LLM providers, you end up juggling:

- Multiple API keys scattered across tools and configs
- No single view of how many tokens you've used (and where)
- Manual switching between providers when testing models

**LLM API Proxy** solves this by acting as a single OpenAI-compatible endpoint. Your client (VS Code, Cursor, shell scripts, etc.) talks to the proxy using one key — the proxy routes requests to the right backend based on the model name prefix you choose.

---

## Features

- **Unified OpenAI-compatible API** — `/v1/chat/completions`, `/v1/models`, and `/v1/responses` work exactly like OpenAI's API, so any OpenAI-compatible client works out of the box.
- **Anthropic Messages API** — `/v1/messages` accepts Anthropic-style requests, letting Anthropic-compatible clients target any backend through the proxy.
- **Direct Anthropic backend** — Native `anthropic` backend type connects directly to the Anthropic API.
- **Multi-backend routing** — Route to different providers based on model name prefix (e.g. `zai/glm-5.1`, `openrouter/anthropic/claude-sonnet-4`). Four strategies: priority, round-robin, race, and staggered-race.
- **Circuit breaker** — Automatically suspend backends that hit rate limits (429), with configurable thresholds and cooldowns.
- **Streaming support** — Full SSE streaming for chat completions and Anthropic messages.
- **OAuth backends** — Use GitHub Copilot and OpenAI Codex without managing API keys. Both authenticate through the web UI.
- **Native Responses API** — Codex backends support the native `/v1/responses` endpoint for passthrough to the OpenAI Responses API.
- **Identity spoofing** — Make outgoing requests look like they came from CLI tools (Codex CLI, Claude Code, Gemini CLI, etc.) with built-in or custom profiles.
- **Token & latency tracking** — Every request recorded with prompt tokens, completion tokens, reasoning tokens, time-to-first-token, throughput, and latency. Stats persist to SQLite.
- **Persistent chat sessions** — Built-in chat interface with session management, auto-generated titles, and multi-turn conversations stored in SQLite.
- **Web dashboard** — Live stats, request history, per-backend token breakdown, backend management, OAuth status, config editor, circuit breaker monitoring, and an interactive playground.
- **Web UI authentication** — Optional username/password login with user management CLI.
- **Hot reload** — Edit and save config from the UI or send `SIGHUP`; backends reload without restarting.
- **Single binary** — SQLite for persistent stats, no external dependencies. Just a YAML config file.
- **Backend toggling** — Enable or disable backends from the UI without deleting them from config.
- **Docker-ready** — Multi-stage Dockerfile with Docker Compose support.

---

## Supported Providers

Any OpenAI-compatible or Anthropic-compatible HTTP API works. Built-in support for:

| Provider                                              | Backend type | Auth method             | Notes                                                |
| ----------------------------------------------------- | ------------ | ----------------------- | ---------------------------------------------------- |
| [Z.ai](https://z.ai)                                  | `openai`     | API key                 | GLM models; separate general & coding plan endpoints |
| [OpenRouter](https://openrouter.ai)                   | `openai`     | API key                 | 200+ models from many providers                      |
| [OpenCode Zen](https://opencode.ai)                   | `openai`     | API key                 | Curated coding models, pay-as-you-go                 |
| [OpenCode Go](https://opencode.ai)                    | `openai`     | API key                 | Subscription tier for coding models                  |
| [GitHub Copilot](https://github.com/features/copilot) | `copilot`    | GitHub Device Code Flow | Authenticate via the web UI                          |
| [OpenAI Codex](https://openai.com/codex/)             | `codex`      | OAuth PKCE flow         | Web-based login via settings UI                      |
| [Anthropic](https://anthropic.com)                    | `anthropic`  | API key                 | Direct Anthropic Messages API                        |
| Any OpenAI-compatible API                             | `openai`     | API key                 | Self-hosted, Azure, Ollama, vLLM, etc.               |

---

## Quick Start

### Prerequisites

- [Go 1.25+](https://go.dev/dl/) or [Docker](https://docs.docker.com/get-docker/)

### 1. Clone & Build

```bash
git clone https://github.com/menno/llmapiproxy
cd llmapiproxy
make build
```

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` — at minimum, set your API keys:

```yaml
server:
  port: 8000
  api_keys:
    - "my-secret-proxy-key"

backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."

  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "your-zai-api-key"
    models:
      - glm-5.1
      - glm-5-turbo
```

### 3. Run

```bash
./llmapiproxy serve
```

The proxy listens on port `8000` by default. Open [http://localhost:8000](http://localhost:8000) to see the dashboard.

### 4. Make a Request

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer my-secret-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/openai/gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Docker

```bash
cp docker-compose.example.yml docker-compose.yml
docker compose up -d
```

See [Docker Deployment](docs/docker.md) for details.

---

## API Endpoints

| Endpoint               | Method | Description                                         |
| ---------------------- | ------ | --------------------------------------------------- |
| `/v1/chat/completions` | POST   | OpenAI-compatible chat completions                  |
| `/v1/messages`         | POST   | Anthropic Messages API compatible                   |
| `/v1/responses`        | POST   | Native Responses API passthrough (Codex)            |
| `/v1/models`           | GET    | List available models (`?mode=flat` or `?mode=raw`) |
| `/health`              | GET    | Health check with OAuth backend status              |

### Model Naming

Use bare model IDs — the proxy routes to the right backend based on your routing config:

```
glm-5.1
gpt-5.4
claude-sonnet-4
kimi-k2.5
```

To explicitly target a specific backend, prefix the model with the backend name:

```
zai/glm-5.1
openrouter/anthropic/claude-sonnet-4
copilot/gpt-4o
codex/gpt-5.3-codex
```

### Configure Editors

Set the API base URL to `http://localhost:8000/v1` and the API key to your proxy key. Works with VS Code, Cursor, Continue.dev, aichat, and any OpenAI-compatible client.

---

## Web Dashboard

Navigate to [http://localhost:8000/ui/](http://localhost:8000/ui/) for the web dashboard.

| Page       | Path             | Description                                             |
| ---------- | ---------------- | ------------------------------------------------------- |
| Dashboard  | `/ui/`           | Live request feed, token totals, circuit breaker status |
| Models     | `/ui/models`     | Browse models, enable/disable backends, refresh cache   |
| Chat       | `/ui/chat`       | Persistent chat sessions with auto-generated titles     |
| Playground | `/ui/playground` | Quick model testing with streaming                      |
| Settings   | `/ui/settings`   | API keys, OAuth login, config editor, stats controls    |

---

## Documentation

Full documentation is available in the [`docs/`](docs/) directory:

| Guide                                            | Description                                 |
| ------------------------------------------------ | ------------------------------------------- |
| [Getting Started](docs/getting-started.md)       | Install, configure, and run                 |
| [Configuration Reference](docs/configuration.md) | Complete YAML options                       |
| [API Reference](docs/api.md)                     | All endpoints and parameters                |
| [Provider Setup Guides](docs/providers.md)       | Backend-specific instructions               |
| [Routing & Failover](docs/routing.md)            | Priority, round-robin, race, staggered-race |
| [Circuit Breaker](docs/circuit-breaker.md)       | Automatic rate-limit suspension             |
| [Identity Spoofing](docs/identity.md)            | CLI tool request signatures                 |
| [Chat & Playground](docs/chat-and-playground.md) | Persistent chat sessions                    |
| [Authentication & Users](docs/authentication.md) | API keys, web UI auth, user CLI             |
| [Docker Deployment](docs/docker.md)              | Production container setup                  |
| [Development Guide](docs/development.md)         | Build, test, contribute                     |

---

## Project Structure

```
cmd/llmapiproxy/           # Application entrypoint, CLI commands
internal/
  backend/                 # Backend interface + OpenAI/Anthropic/Copilot/Codex implementations
  chat/                    # Persistent chat session storage
  circuit/                 # Circuit breaker for rate-limit protection
  config/                  # YAML config loading, hot-reload, file watching
  identity/                # Identity spoofing profiles
  logger/                  # Zerolog initialization
  oauth/                   # OAuth flows (Copilot Device Code, Codex PKCE)
  proxy/                   # HTTP handlers, auth middleware
  quota/                   # Per-provider quota tracking
  stats/                   # In-memory collector + SQLite storage
  users/                   # Web UI user authentication
  web/                     # Dashboard, templates, static assets
docs/                      # Documentation
```

## License

MIT
