# API Reference

The proxy exposes an OpenAI-compatible API at `/v1/*` and a web dashboard at `/ui/*`.

## Authentication

Include your proxy API key in the `Authorization` header:

```
Authorization: Bearer your-proxy-api-key
```

## OpenAI-Compatible Endpoints

### POST /v1/chat/completions

Create a chat completion. Fully compatible with OpenAI's API.

**Request:**

```bash
curl http://localhost:8080/v1/chat/completions \
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

| Parameter     | Type          | Required | Description                        |
| ------------- | ------------- | -------- | ---------------------------------- |
| `model`       | string        | yes      | Model ID in `backend/model` format |
| `messages`    | array         | yes      | Array of message objects           |
| `stream`      | boolean       | no       | Enable SSE streaming               |
| `temperature` | number        | no       | Sampling temperature               |
| `max_tokens`  | number        | no       | Maximum tokens to generate         |
| `tools`       | array         | no       | Tools/functions for agent use      |
| `tool_choice` | string/object | no       | Tool selection strategy            |

**Response:**

Standard OpenAI chat completion format.

### GET /v1/models

List all available models from all enabled backends.

```bash
curl http://localhost:8080/v1/models \
  -H "Authorization: Bearer your-key"
```

**Query Parameters:**

| Parameter | Type   | Default    | Description                                                                                           |
| --------- | ------ | ---------- | ----------------------------------------------------------------------------------------------------- |
| `mode`    | string | `flat`     | `flat` — deduplicated models with routing metadata (default). `raw` — all models with backend prefix. |

**Response (default / `?mode=flat`):**

Models from multiple backends are deduplicated by base ID and merged. When the same model is available on multiple backends, the response includes `available_backends` (routing priority order) and `routing_strategy` so the proxy can automatically select the best backend.

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

Each model is listed per-backend with a backend-prefixed ID (e.g. `openrouter/gpt-4o`). No deduplication is performed — the caller selects the exact backend and model. Each entry includes `available_backends` (single element) and `routing_strategy: "direct"`.

```bash
curl "http://localhost:8080/v1/models?mode=raw" \
  -H "Authorization: Bearer your-key"
```

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
    },
    {
      "id": "copilot/gpt-4o",
      "object": "model",
      "owned_by": "copilot",
      "available_backends": ["copilot"],
      "routing_strategy": "direct"
    }
  ]
}
```

## Dashboard API

These endpoints serve the web UI. They return HTML by default, JSON where noted.

### GET /ui/

Main dashboard page (HTML).

### GET /ui/dashboard/data

Dashboard data (JSON).

```json
{
  "stats": {
    "total_requests": 1250,
    "total_tokens": 450000,
    "avg_latency_ms": 420
  },
  "backends": [...],
  "recent_requests": [...]
}
```

### GET /ui/models

Models management page (HTML).

### GET /ui/backends/{name}/models

Models for a specific backend (JSON).

### GET /ui/playground

Interactive playground page (HTML).

### GET /ui/playground/models

All models for playground dropdown (JSON).

### GET /ui/analytics

Analytics page (HTML).

### GET /ui/analytics/data

Analytics data (JSON) with query parameters:

| Parameter | Description                                 |
| --------- | ------------------------------------------- |
| `window`  | Time window: `1h`, `6h`, `24h`, `7d`, `30d` |
| `backend` | Filter by backend name                      |
| `model`   | Filter by model ID                          |
| `client`  | Filter by client name                       |

Response includes summary, percentiles, time series, and rankings.

### GET /ui/settings

Settings page (HTML).

### POST /ui/config/save

Save raw config YAML. Form field: `config`.

### POST /ui/settings/clear-stats

Clear all stats data.

### POST /ui/settings/toggle-stats

Toggle stats collection on/off.

**Body:** `enabled=true` or `enabled=false`

### POST /ui/settings/keys/add

Add an API key.

**Body:**

- `key`: The API key to add

### POST /ui/settings/keys/delete

Delete an API key.

**Body:**

- `key`: The API key to remove

### POST /ui/settings/backends/toggle

Enable/disable a backend.

**Body:**

- `name`: Backend name
- `enabled`: `true` or `false`

### POST /ui/settings/clients/add

Add a named client.

**Body:**

- `name`: Client name
- `api_key`: Client's API key

### POST /ui/settings/clients/delete

Delete a client.

**Body:**

- `name`: Client name

### POST /ui/settings/model-cache-ttl

Update the model cache TTL.

**Body:**

- `ttl`: Duration string (e.g., `5m`, `1h`, `0`)

### POST /ui/backends/{name}/refresh-models

Clear the cached model list for a specific backend, forcing a fresh fetch.

**Response:** Redirects back to the models page.

### POST /ui/routing/save

Save routing configuration.

**Body:**

- `model`: Model pattern
- `backends`: Comma-separated list of backends

### GET /ui/stats/detail

Request detail view. Query parameter: `id=<request-id>`.

## Health & Misc

### GET /health

Health check.

**Response:**

```json
{ "status": "ok" }
```

### GET /

Redirects to `/ui/`.

### GET /ui/static/\*

Static assets (CSS, JS, icons).

## Error Responses

Errors follow OpenAI format:

```json
{
  "error": {
    "message": "Invalid API key",
    "type": "authentication_error"
  }
}
```

Common status codes:

| Code | Meaning                    |
| ---- | -------------------------- |
| 200  | Success                    |
| 401  | Invalid API key            |
| 404  | Backend or model not found |
| 500  | Backend error              |
| 502  | Backend unavailable        |
