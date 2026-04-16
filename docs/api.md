# API Reference

The proxy exposes an OpenAI-compatible API at `/v1/*`, an Anthropic-compatible endpoint at `/v1/messages`, and a web dashboard at `/ui/*`.

## Authentication

Include your proxy API key in the `Authorization` header:

```
Authorization: Bearer your-proxy-api-key
```

Alternatively, use the `x-api-key` header:

```
x-api-key: your-proxy-api-key
```

## OpenAI-Compatible Endpoints

### POST /v1/chat/completions

Create a chat completion. Fully compatible with the OpenAI Chat Completions API.

**Request:**

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "openrouter/gpt-4o",
    "messages": [
      {"role": "user", "content": "Hello!"}
    ],
    "stream": false
  }'
```

**Parameters:**

| Parameter               | Type          | Required | Description                                  |
| ----------------------- | ------------- | -------- | -------------------------------------------- |
| `model`                 | string        | yes      | Model ID in `backend/model` format           |
| `messages`              | array         | yes      | Array of message objects                     |
| `stream`                | boolean       | no       | Enable SSE streaming                         |
| `temperature`           | number        | no       | Sampling temperature                         |
| `max_tokens`            | number        | no       | Maximum tokens to generate                   |
| `max_completion_tokens` | number        | no       | Alternative max tokens (used by some models) |
| `tools`                 | array         | no       | Tools/functions for agent use                |
| `tool_choice`           | string/object | no       | Tool selection strategy                      |
| `top_p`                 | number        | no       | Top-p sampling                               |

**Response:** Standard OpenAI chat completion format.

### POST /v1/messages

Anthropic Messages API compatible endpoint. Accepts Anthropic-style requests and translates them to the internal format for routing to any backend.

**Request:**

```bash
curl http://localhost:8000/v1/messages \
  -H "x-api-key: your-proxy-api-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "zai/glm-5.1",
    "max_tokens": 512,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

**Parameters:**

| Parameter    | Type    | Required | Description                              |
| ------------ | ------- | -------- | ---------------------------------------- |
| `model`      | string  | yes      | Model ID in `backend/model` format       |
| `messages`   | array   | yes      | Array of Anthropic-style message objects |
| `max_tokens` | int     | yes      | Maximum tokens to generate (must be > 0) |
| `stream`     | boolean | no       | Enable SSE streaming                     |

**Response:** Anthropic Messages API format with full SSE streaming support.

### POST /v1/responses

Native Responses API passthrough for Codex backends. Forwards requests directly to the OpenAI Responses API.

**Request:**

```bash
curl http://localhost:8000/v1/responses \
  -H "Authorization: Bearer your-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "codex/gpt-5.3-codex",
    "input": "Write a function that reverses a linked list."
  }'
```

### GET /v1/models

List all available models from all enabled backends.

```bash
curl http://localhost:8000/v1/models \
  -H "Authorization: Bearer your-key"
```

**Query Parameters:**

| Parameter | Type   | Default | Description                                                                                 |
| --------- | ------ | ------- | ------------------------------------------------------------------------------------------- |
| `mode`    | string | `flat`  | `flat` — deduplicated models with routing metadata. `raw` — all models with backend prefix. |

**Response (default / `?mode=flat`):**

Models from multiple backends are deduplicated by base ID and merged. When the same model is available on multiple backends, the response includes `available_backends` (routing priority order) and `routing_strategy`:

```json
{
  "object": "list",
  "data": [
    {
      "id": "gpt-4o",
      "object": "model",
      "owned_by": "openrouter",
      "display_name": "GPT-4o",
      "available_backends": ["openrouter", "copilot"],
      "routing_strategy": "priority"
    }
  ]
}
```

**Response (`?mode=raw`):**

Each model is listed per-backend with a backend-prefixed ID. No deduplication:

```json
{
  "object": "list",
  "data": [
    {
      "id": "openrouter/gpt-4o",
      "object": "model",
      "owned_by": "openrouter",
      "available_backends": ["openrouter"],
      "routing_strategy": "direct"
    }
  ]
}
```

### GET /health

Health check endpoint. Returns JSON with overall status and per-backend OAuth reauth requirements:

```json
{
  "status": "ok",
  "backends": {
    "copilot": { "needs_reauth": false },
    "codex": { "needs_reauth": true }
  }
}
```

Status is `"degraded"` if any OAuth backend requires re-authentication.

---

## Model Naming

Use bare model IDs — the proxy routes to the right backend based on your [routing config](routing.md):

```
glm-5.1
gpt-5.4
claude-sonnet-4
kimi-k2.5
o3
```

To explicitly target a specific backend, prefix the model with the backend name (`<backend-name>/<model-id>`):

```
zai/glm-5.1
zai-coding/glm-5-turbo
openrouter/anthropic/claude-sonnet-4
zen/kimi-k2.5
copilot/gpt-4o
codex/gpt-5.3-codex
```

Prefix routing does **not** check `SupportsModel()` — the request is forwarded directly to the named backend. If no prefix is given, the proxy resolves the model via routing config or wildcard matching (see [Routing & Failover](routing.md)).

---

## Dashboard API

These endpoints serve the web UI. They return HTML by default; some return JSON as noted.

### Pages

| Endpoint             | Method | Description                        |
| -------------------- | ------ | ---------------------------------- |
| `/ui/`               | GET    | Main dashboard (HTML)              |
| `/ui/dashboard/data` | GET    | Dashboard stats data (JSON)        |
| `/ui/models`         | GET    | Models management page (HTML)      |
| `/ui/playground`     | GET    | Interactive playground (HTML)      |
| `/ui/chat`           | GET    | Chat sessions page (HTML)          |
| `/ui/analytics`      | GET    | Analytics (redirects to dashboard) |
| `/ui/settings`       | GET    | Settings page (HTML)               |

### Data Endpoints

| Endpoint                              | Method | Description                                      |
| ------------------------------------- | ------ | ------------------------------------------------ |
| `/ui/stats`                           | GET    | Stats fragment (HTMX)                            |
| `/ui/stats/cards`                     | GET    | Stats summary cards                              |
| `/ui/stats/detail`                    | GET    | Request detail view                              |
| `/ui/backends/{name}/models`          | GET    | Models for a specific backend (JSON)             |
| `/ui/backends/{name}/upstream-models` | GET    | All upstream models for backend (JSON)           |
| `/ui/playground/models`               | GET    | Models for playground dropdown (JSON)            |
| `/ui/chat/models`                     | GET    | Models for chat dropdown (JSON)                  |
| `/ui/routing/config`                  | GET    | Current routing config (JSON)                    |
| `/ui/routing/backend-fallbacks`       | GET    | Backend fallback matrix (JSON)                   |
| `/ui/oauth/status`                    | GET    | OAuth token status for all OAuth backends (JSON) |

### Management Endpoints

| Endpoint                               | Method | Description                               |
| -------------------------------------- | ------ | ----------------------------------------- |
| `/ui/config/save`                      | POST   | Save raw config YAML                      |
| `/ui/settings/clear-stats`             | POST   | Clear all stats data                      |
| `/ui/settings/toggle-stats`            | POST   | Toggle stats collection                   |
| `/ui/settings/model-cache-ttl`         | POST   | Update cache TTL                          |
| `/ui/settings/server`                  | POST   | Update server config (host, port, domain) |
| `/ui/settings/keys/add`                | POST   | Add an API key                            |
| `/ui/settings/keys/delete`             | POST   | Delete an API key                         |
| `/ui/settings/clients/add`             | POST   | Add a client config                       |
| `/ui/settings/clients/delete`          | POST   | Delete a client config                    |
| `/ui/settings/backends/toggle`         | POST   | Enable/disable a backend                  |
| `/ui/settings/backends/switch-type`    | POST   | Change backend type                       |
| `/ui/settings/backends/disabled-model` | POST   | Toggle model disable list                 |
| `/ui/settings/backends/add`            | POST   | Add backend via UI                        |
| `/ui/settings/backends/delete`         | POST   | Delete backend via UI                     |
| `/ui/backends/{name}/refresh-models`   | POST   | Force refresh model cache                 |
| `/ui/routing/save`                     | POST   | Update routing config                     |
| `/ui/analytics/wipe`                   | POST   | Clear all analytics data                  |

### OAuth Endpoints

| Endpoint                               | Method | Description                      |
| -------------------------------------- | ------ | -------------------------------- |
| `/ui/oauth/login/{backend}`            | GET    | Initiate OAuth login             |
| `/ui/oauth/device-login/{backend}`     | GET    | Start device code flow (Copilot) |
| `/ui/oauth/device-code-info/{backend}` | GET    | Get device code details          |
| `/ui/oauth/callback/{backend}`         | GET    | OAuth callback handler           |
| `/ui/oauth/disconnect/{backend}`       | POST   | Clear stored tokens              |
| `/ui/oauth/check-status/{backend}`     | POST   | Poll token status                |

### Circuit Breaker Endpoints

| Endpoint                   | Method | Description                       |
| -------------------------- | ------ | --------------------------------- |
| `/ui/circuit/card`         | GET    | Circuit breaker card (HTMX)       |
| `/ui/circuit/states`       | GET    | All circuit breaker states (JSON) |
| `/ui/circuit/reset/{name}` | POST   | Reset a specific circuit breaker  |
| `/ui/circuit/reset-all`    | POST   | Reset all circuit breakers        |
| `/ui/circuit/config`       | POST   | Update circuit breaker thresholds |

### Chat Session Endpoints

| Endpoint                          | Method | Description                     |
| --------------------------------- | ------ | ------------------------------- |
| `/ui/chat/sessions`               | GET    | List all chat sessions          |
| `/ui/chat/sessions`               | POST   | Create new session              |
| `/ui/chat/sessions/{id}`          | GET    | Get session details             |
| `/ui/chat/sessions/{id}`          | PUT    | Update session (rename)         |
| `/ui/chat/sessions/{id}`          | DELETE | Delete session                  |
| `/ui/chat/sessions`               | DELETE | Delete all sessions             |
| `/ui/chat/sessions/{id}/messages` | GET    | List messages in session        |
| `/ui/chat/sessions/{id}/messages` | POST   | Save message to session         |
| `/ui/chat/sessions/{id}/title`    | POST   | Generate session title via LLM  |
| `/ui/chat/title-model`            | PUT    | Set title generation model      |
| `/ui/chat/default-model`          | PUT    | Set default model for new chats |

### Identity Profile Endpoints

| Endpoint                               | Method | Description                             |
| -------------------------------------- | ------ | --------------------------------------- |
| `/ui/json/identity-profiles`           | GET    | List available identity profiles (JSON) |
| `/ui/identity-profile`                 | POST   | Set global identity profile             |
| `/ui/backends/{name}/identity-profile` | POST   | Set per-backend identity profile        |

### Web UI Auth Endpoints

| Endpoint     | Method | Description                        |
| ------------ | ------ | ---------------------------------- |
| `/ui/login`  | GET    | Login page                         |
| `/ui/login`  | POST   | Authenticate (sets session cookie) |
| `/ui/setup`  | GET    | First admin user setup page        |
| `/ui/setup`  | POST   | Create first user and auto-login   |
| `/ui/logout` | POST   | Clear session, redirect to login   |
