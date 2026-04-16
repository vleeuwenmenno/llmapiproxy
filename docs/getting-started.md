# Getting Started

Install, configure, and run LLM API Proxy in a few minutes.

## Prerequisites

- [Go 1.25+](https://go.dev/dl/) (for building from source)
- Or [Docker](https://docs.docker.com/get-docker/) (for container deployment)

## Quick Start

### 1. Build

```bash
git clone https://github.com/menno/llmapiproxy
cd llmapiproxy
make build
```

Or with Go directly:

```bash
go build -o llmapiproxy ./cmd/llmapiproxy
```

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` — at minimum, set your API keys and the backends you want to use:

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
```

See [Configuration Reference](configuration.md) for all options and [Provider Setup Guides](providers.md) for backend-specific instructions.

### 3. Run

```bash
./llmapiproxy serve
```

Or with a custom config path:

```bash
./llmapiproxy serve --config /path/to/config.yaml
```

The proxy starts on port `8000` by default. Open [http://localhost:8000](http://localhost:8000) to see the web dashboard.

### 4. Make Your First Request

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer my-secret-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/openai/gpt-4o",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## Using with Editors & Tools

Point any OpenAI-compatible client at `http://localhost:8000/v1` with your proxy API key.

### VS Code / Copilot / Cursor

Set the API base URL to `http://localhost:8000/v1` and the API key to your proxy key. The proxy is fully transparent — the client never needs to know which backend is handling the request.

### Continue.dev

`.continue/config.json`:

```json
{
  "models": [
    {
      "title": "Proxy - Claude",
      "provider": "openai",
      "model": "openrouter/anthropic/claude-sonnet-4",
      "apiBase": "http://localhost:8000/v1",
      "apiKey": "your-proxy-api-key"
    }
  ]
}
```

### Shell (aichat)

`~/.config/aichat/config.yaml`:

```yaml
clients:
  - type: openai
    api_base: http://localhost:8000/v1
    api_key: your-proxy-api-key
    models:
      - name: openrouter/gpt-4o
```

## Docker

See the [Docker Deployment Guide](docker.md) for production setup with Docker Compose.

## Next Steps

- **[Configuration Reference](configuration.md)** — all YAML options
- **[Provider Setup Guides](providers.md)** — backend-specific instructions for OpenRouter, Z.ai, Copilot, Codex, Anthropic, and more
- **[Routing & Failover](routing.md)** — priority, round-robin, race, and staggered-race strategies
- **[API Reference](api.md)** — endpoint documentation
- **[Chat & Playground](chat-and-playground.md)** — persistent chat sessions in the browser
