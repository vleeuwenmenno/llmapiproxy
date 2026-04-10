# LLM API Proxy

> All your LLM providers in one central place.

A lightweight, self-hosted proxy that aggregates multiple LLM provider APIs (Z.ai, OpenRouter, OpenCode Zen, Codex, and any OpenAI-compatible endpoint) behind a single unified API. Keep one API key in your tools, track token usage across all providers, and manage everything from a built-in web dashboard.

---

## Why?

If you have subscriptions at several LLM providers, you end up juggling:

- Multiple API keys scattered across tools and configs
- No single view of how many tokens you've used (and where)
- Manual switching between providers when testing models

**LLM API Proxy** solves this by acting as a single OpenAI-compatible endpoint. Your client (VS Code, Cursor, shell scripts, etc.) talks to the proxy using one key — the proxy routes requests to the right backend based on the model name prefix you choose.

---

## Features

- **Unified OpenAI-compatible API** — `/v1/chat/completions` and `/v1/models` work exactly like OpenAI's API, so any OpenAI-compatible client works out of the box.
- **Multi-backend routing** — route to different providers based on model name prefix (e.g. `zai/glm-5.1`, `openrouter/anthropic/claude-sonnet-4`).
- **Streaming support** — SSE streaming is fully proxied to clients.
- **Token & latency tracking** — every request is recorded with prompt tokens, completion tokens, latency, backend, and model.
- **Web dashboard** — live stats, request history, per-backend token breakdown, and an in-browser config editor.
- **Hot reload** — edit and save your config from the UI; backends reload without restarting the server.
- **Single binary** — no database, no external dependencies. Just a YAML config file.

---

## Supported Providers

Any OpenAI-compatible HTTP API works. The example config includes:

| Provider | Backend type | Notes |
|---|---|---|
| [Z.ai](https://z.ai) | `openai` | GLM models; separate general & coding plan endpoints |
| [OpenRouter](https://openrouter.ai) | `openai` | 200+ models from many providers |
| [OpenCode Zen](https://opencode.ai) | `openai` | Curated coding models, pay-as-you-go |
| [OpenCode Go](https://opencode.ai) | `openai` | Subscription tier for coding models |
| Any OpenAI-compatible API | `openai` | Self-hosted, Azure OpenAI, etc. |

---

## Quick Start

### Prerequisites

- [Go 1.21+](https://go.dev/dl/)

### 1. Clone & build

```bash
git clone https://github.com/menno/llmapiproxy
cd llmapiproxy
go build -o llmapiproxy ./cmd/llmapiproxy
```

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and fill in your provider API keys:

```yaml
server:
  listen: ":8080"
  api_keys:
    - "my-secret-proxy-key"   # clients use this to authenticate

backends:
  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "your-zai-api-key"
    models:
      - glm-5.1
      - glm-5-turbo

  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."
```

### 3. Run

```bash
./llmapiproxy -config config.yaml
```

The proxy listens on `:8080` by default. Open [http://localhost:8080](http://localhost:8080) to see the dashboard.

---

## Using the API

Point any OpenAI-compatible client at `http://localhost:8080` with your proxy API key.

### Model naming

Models are addressed as `<backend-name>/<model-id>`:

```
zai/glm-5.1
zai-coding/glm-5-turbo
openrouter/anthropic/claude-sonnet-4
zen/kimi-k2.5
```

### Example — curl

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer my-secret-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "zai/glm-5.1",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Example — list available models

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer my-secret-proxy-key"
```

### Configure VS Code / Copilot / Cursor

Set the API base URL to `http://localhost:8080/v1` and the API key to your proxy key. The proxy is fully transparent — the client never needs to know which backend is handling the request.

---

## Web Dashboard

Navigate to [http://localhost:8080/ui/](http://localhost:8080/ui/) to access the dashboard.

| Page | Path | Description |
|---|---|---|
| Dashboard | `/ui/` | Live request feed, token totals, per-backend breakdown, latency stats |
| Config editor | `/ui/config` | Edit `config.yaml` in the browser and save with hot reload |

Stats auto-refresh every few seconds. Up to 10,000 recent requests are kept in memory.

---

## Configuration Reference

```yaml
server:
  # Address to listen on (default: :8080)
  listen: ":8080"

  # One or more API keys clients must send in the Authorization: Bearer header
  api_keys:
    - "your-proxy-api-key"

  # Optional: restrict the web UI config editor with a separate key
  # admin_key: "your-admin-key"

backends:
  - name: my-backend          # used as the model prefix
    type: openai              # only supported type currently
    base_url: https://...     # provider API base URL
    api_key: "..."            # your provider API key

    # Optional: restrict to specific models (if omitted, all models are accepted)
    models:
      - model-id-1
      - model-id-2

    # Optional: additional HTTP headers forwarded to the backend
    extra_headers:
      HTTP-Referer: "https://my-app.example.com"
```

---

## Security Notes

- Bind to `localhost` (or a private network) when possible. The web UI does not require authentication unless `admin_key` is set.
- Never commit `config.yaml` with real API keys to version control — use `config.example.yaml` as a template.
- API keys are validated on every request to `/v1/*` endpoints.

---

## Project Structure

```
cmd/llmapiproxy/    — main entry point, server setup, routing
internal/
  backend/          — Backend interface, OpenAI HTTP client, registry & model routing
  config/           — YAML config loading, validation, hot-reload manager
  proxy/            — HTTP handler (chat completions, model listing), auth middleware
  stats/            — In-memory request/token collector and aggregator
  web/              — Dashboard UI, config editor (HTMX + Go templates)
```

---

## License

MIT
