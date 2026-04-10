# LLM API Proxy

> One API key. Every provider. Built-in dashboard.

A lightweight proxy that unifies multiple LLM backends behind a single OpenAI-compatible endpoint. Route requests by model prefix, track usage across providers, and manage everything from a web UI.

Supports any OpenAI-compatible API. Native backend types for Codex, GitHub Copilot, and Claude's API are planned.

---

## Quick Start

### Install

```bash
go install github.com/vleeuwenmenno/llmapiproxy/cmd/llmapiproxy@latest
```

Or build from source:

```bash
git clone https://github.com/vleeuwenmenno/llmapiproxy
cd llmapiproxy
go build -o llmapiproxy ./cmd/llmapiproxy
```

### Configure

```bash
cp config.example.yaml config.yaml
# Edit config.yaml with your provider API keys
```

Minimal example:

```yaml
server:
  listen: ":8080"
  api_keys: ["your-proxy-api-key"]

backends:
  - name: openrouter
    type: openai # OpenAI-compatible API
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."
```

### Run

```bash
llmapiproxy -config config.yaml
```

Open [http://localhost:8080/ui/](http://localhost:8080/ui/) for the dashboard.

---

## Usage

### Model Naming

Address models as `<backend>/<model-id>`:

```
openrouter/anthropic/claude-sonnet-4
zai/glm-5.1
```

### Example Request

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer your-proxy-api-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/anthropic/claude-sonnet-4",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Configure Your Editor

Set the API base URL to `http://localhost:8080/v1` and use your proxy API key. See [editor setup guides](docs/providers.md#editor-configuration).

---

## Features

- **OpenAI-compatible API** — `/v1/chat/completions` and `/v1/models` endpoints
- **Multi-backend routing** — Route by model prefix with failover support
- **Streaming** — Full SSE proxying
- **Usage tracking** — Token counts, latency, per-backend breakdown. SQLite persistence.
- **Web dashboard** — Live stats, request history, config editor, playground
- **Hot reload** — Config changes apply without restart
- **Client system** — Named API keys with per-backend overrides
- **Model failover** — Configure fallback backends per model

---

## Documentation

- [Configuration Reference](docs/configuration.md) — Complete config options
- [Provider Setup](docs/providers.md) — Guides for OpenRouter, Z.ai, OpenCode, etc.
- [Features](docs/features.md) — Clients, routing, analytics, playground
- [API Reference](docs/api.md) — Endpoints and authentication

---

## Security

- Bind to `localhost` in `config.yaml` for local-only access
- API keys validated on every request
- Never commit `config.yaml` with real keys

---

## License

MIT
