# LLM API Proxy

> All your LLM providers in one central place.

A lightweight, self-hosted proxy that aggregates multiple LLM provider APIs (Z.ai, OpenRouter, OpenCode Zen, GitHub Copilot, OpenAI Codex, and any OpenAI-compatible endpoint) behind a single unified API. Keep one API key in your tools, track token usage across all providers, and manage everything from a built-in web dashboard.

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
- **Anthropic Messages compatibility** — `/v1/messages` accepts Anthropic-style requests for text-first workflows, which lets Anthropic-compatible clients target backends like Z.ai GLM models through the same proxy.
- **Multi-backend routing** — route to different providers based on model name prefix (e.g. `zai/glm-5.1`, `openrouter/anthropic/claude-sonnet-4`).
- **Streaming support** — SSE streaming is fully proxied to clients.
- **OAuth backends** — use GitHub Copilot and OpenAI Codex without managing API keys. Copilot uses a GitHub Device Code Flow; Codex uses an OAuth PKCE flow. Both are managed through the web UI.
- **Native Responses API** — Codex backends support the native `/v1/responses` endpoint for passthrough to the OpenAI Responses API.
- **Token & latency tracking** — every request is recorded with prompt tokens, completion tokens, latency, backend, and model. Stats persist to SQLite across restarts.
- **Web dashboard** — live stats, request history, per-backend token breakdown, backend management, OAuth status, and an in-browser config editor.
- **Hot reload** — edit and save your config from the UI; backends reload without restarting the server.
- **Single binary** — SQLite for persistent stats, no external dependencies. Just a YAML config file.
- **Backend toggling** — enable or disable backends from the UI without deleting them from config.

---

## Supported Providers

Any OpenAI-compatible HTTP API works. The example config includes:

| Provider                            | Backend type | Auth method                  | Notes                                                |
| ----------------------------------- | ------------ | ---------------------------- | ---------------------------------------------------- |
| [Z.ai](https://z.ai)                | `openai`     | API key                      | GLM models; separate general & coding plan endpoints |
| [OpenRouter](https://openrouter.ai) | `openai`     | API key                      | 200+ models from many providers                      |
| [OpenCode Zen](https://opencode.ai) | `openai`     | API key                      | Curated coding models, pay-as-you-go                 |
| [OpenCode Go](https://opencode.ai)  | `openai`     | API key                      | Subscription tier for coding models                  |
| [GitHub Copilot](https://github.com/features/copilot) | `copilot` | GitHub Device Code Flow | Authenticate via the web UI — no local tools needed |
| [OpenAI Codex](https://openai.com/codex/) | `codex` | OAuth PKCE flow       | Web-based login flow managed through the settings UI |
| Any Anthropic-compatible API        | `anthropic`  | API key                      | Uses upstream `/v1/messages` and `/v1/models`        |
| Any OpenAI-compatible API           | `openai`     | API key                      | Self-hosted, Azure OpenAI, etc.                      |

---

## Quick Start

### Prerequisites

- [Go 1.22+](https://go.dev/dl/)

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
    - "my-secret-proxy-key" # clients use this to authenticate

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
copilot/gpt-4o
codex/gpt-5.3-codex
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

### Example — Codex Responses API

The `/v1/responses` endpoint passes requests through natively to Codex backends:

```bash
curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer my-secret-proxy-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex/gpt-5.3-codex",
    "input": "Write a function that reverses a linked list."
  }'
```

### Example — list available models

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer my-secret-proxy-key"
```

### Example — Anthropic Messages API

```bash
curl http://localhost:8080/v1/messages \
  -H "x-api-key: my-secret-proxy-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "zai/glm-5.1",
    "max_tokens": 512,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Configure VS Code / Copilot / Cursor

Set the API base URL to `http://localhost:8080/v1` and the API key to your proxy key. The proxy is fully transparent — the client never needs to know which backend is handling the request.

---

## OAuth Backends

LLM API Proxy supports two special backend types that authenticate via OAuth instead of static API keys. This lets you use subscription-based services (GitHub Copilot, OpenAI Codex) without manually managing credentials.

### GitHub Copilot (`copilot`)

The Copilot backend uses **GitHub Device Code Flow** for authentication. You initiate login from the web UI — the proxy displays a code and a verification URL. Visit the URL, enter the code, and authorize the application. The proxy exchanges the resulting GitHub token for a Copilot API token automatically.

No local tools (`gh` CLI, environment variables, or token files) are needed. Tokens are long-lived and validated on-demand.

**Configuration:**

```yaml
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
    # No api_key needed — authentication uses Device Code Flow.
    # Optionally override the GitHub OAuth client_id (defaults to Copilot VS Code extension ID):
    # oauth:
    #   client_id: "Iv1.b507a08c87ecfe98"
```

**Prerequisites:**
- A GitHub account with Copilot access (Individual, Business, or Enterprise)

**To authenticate:**
1. Navigate to **Settings** in the web dashboard (`/ui/settings`)
2. Find the Copilot backend in the OAuth section
3. Click **Connect** to start the Device Code Flow
4. Visit the verification URL and enter the displayed code
5. Authorize the application — the proxy stores the token automatically

### OpenAI Codex (`codex`)

The Codex backend uses an **OAuth PKCE flow** for authentication. You initiate the login from the web UI and the proxy handles token storage and refresh automatically.

**Configuration:**

```yaml
backends:
  - name: codex
    type: codex
    base_url: https://chatgpt.com/backend-api/codex
    oauth:
      # Optional. Omit this to use the built-in Codex client id.
      # client_id: "app_EMoamEEZ73f0CkXaXp7hrann"
      scopes:
        - "openid"
        - "profile"
        - "email"
        - "offline_access"
      auth_url: "https://auth.openai.com/oauth/authorize"
      token_url: "https://auth.openai.com/oauth/token"
    # No api_key needed — uses OAuth.
```

**OAuth config fields:**

| Field        | Description                                         |
| ------------ | --------------------------------------------------- |
| `client_id`  | Optional OAuth client identifier override           |
| `scopes`     | List of OAuth scopes to request                     |
| `auth_url`   | Authorization endpoint URL                          |
| `token_url`  | Token exchange endpoint URL                         |
| `token_path` | *(optional)* Directory to store tokens (default: `tokens/`) |

**To authenticate:**
1. Navigate to **Settings** in the web dashboard (`/ui/settings`)
2. Find the Codex backend in the OAuth section
3. Click **Connect** to start the OAuth login flow
4. Complete authentication in the browser — the proxy stores the token

Tokens are persisted to disk and refreshed automatically. You can disconnect a backend from the same settings page to clear stored tokens.

---

## Web Dashboard

Navigate to [http://localhost:8080/ui/](http://localhost:8080/ui/) to access the dashboard.

| Page      | Path           | Description                                                           |
| --------- | -------------- | --------------------------------------------------------------------- |
| Dashboard | `/ui/`         | Live request feed, token totals, per-backend breakdown, latency stats |
| Models    | `/ui/models`   | Browse models, enable/disable backends, quick-connect setup guides    |
| Settings  | `/ui/settings` | API key management, OAuth status & login, raw config editor, stats, appearance |

Stats persist to SQLite across server restarts. The dashboard auto-refreshes every 10 seconds.

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

  # Optional: path to SQLite database for persistent stats (default: stats.db)
  # stats_path: "stats.db"

  # Optional: disable stats tracking entirely
  # disable_stats: false

backends:
  - name: my-backend # used as the model prefix
    type: openai # backend type: openai, copilot, or codex
    base_url: https://... # provider API base URL
    api_key: "..." # your provider API key (not needed for copilot/codex)

    # Optional: set to false to disable this backend without removing it from config
    # enabled: true

    # Optional: restrict to specific models (if omitted, all models are accepted)
    models:
      - model-id-1
      - model-id-2

    # Optional: additional HTTP headers forwarded to the backend
    extra_headers:
      HTTP-Referer: "https://my-app.example.com"

    # Optional: OAuth configuration (codex backend only)
    # oauth:
    #   client_id: "your-client-id"
    #   scopes:
    #     - "openid"
    #   auth_url: "https://auth.example.com/authorize"
    #   token_url: "https://auth.example.com/oauth/token"
    #   token_path: "tokens/"
```

---

## API Endpoints

| Endpoint                | Method | Description                                       |
| ----------------------- | ------ | ------------------------------------------------- |
| `/v1/chat/completions`  | POST   | OpenAI-compatible chat completions (all backends)  |
| `/v1/responses`         | POST   | Native Responses API passthrough (Codex backends)  |
| `/v1/models`            | GET    | List available models across all backends           |
| `/health`               | GET    | Health check with OAuth backend status              |

### OAuth Management Endpoints

| Endpoint                       | Method | Description                               |
| ------------------------------ | ------ | ----------------------------------------- |
| `/ui/oauth/status`             | GET    | OAuth status for all OAuth backends       |
| `/ui/oauth/login/{backend}`    | GET    | Initiate OAuth login for a backend        |
| `/ui/oauth/callback/{backend}` | GET    | OAuth callback handler                    |
| `/ui/oauth/disconnect/{backend}` | POST | Clear stored tokens for a backend         |

---

## Security Notes

- Bind to `localhost` (or a private network) when possible. The web UI does not require authentication unless `admin_key` is set.
- Never commit `config.yaml` with real API keys to version control — use `config.example.yaml` as a template.
- API keys are validated on every request to `/v1/*` endpoints.
- OAuth tokens are stored locally on disk in the `tokens/` directory. Ensure this directory is not publicly accessible.

---

## Project Structure

```
cmd/llmapiproxy/    — main entry point, server setup, routing
internal/
  backend/          — Backend interface, OpenAI/Copilot/Codex implementations, registry & model routing
  config/           — YAML config loading, validation, hot-reload manager
  oauth/            — OAuth token storage, GitHub Copilot token exchange, Codex PKCE flow
  proxy/            — HTTP handler (chat completions, responses, model listing), auth middleware
  stats/            — In-memory collector + SQLite persistent storage
  web/              — Dashboard, Models, Settings UI (HTMX + Go templates)
```

---

## License

MIT
