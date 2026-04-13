# Configuration Reference

Configuration is loaded from a YAML file specified with the `-config` flag (default: `config.yaml`).

## Server Options

```yaml
server:
  listen: ":8080" # Address to listen on
  api_keys: # One or more keys for client auth
    - "proxy-key-1"
    - "proxy-key-2"
  admin_key: "admin-key" # Optional: key for config editor access
  stats_path: "stats.db" # SQLite database path
  disable_stats: false # Set to true to disable tracking
  model_cache_ttl: "5m" # Cache upstream model lists for 5 minutes
```

| Option            | Type     | Default    | Description                                                                      |
| ----------------- | -------- | ---------- | -------------------------------------------------------------------------------- |
| `listen`          | string   | `:8080`    | Address/port to bind to                                                          |
| `api_keys`        | list     | required   | API keys clients must provide in `Authorization: Bearer <key>` header            |
| `admin_key`       | string   | -          | Optional separate key for web UI config editor access                            |
| `stats_path`      | string   | `stats.db` | Path to SQLite database for persistent stats                                     |
| `disable_stats`   | bool     | `false`    | Disable stats collection entirely                                                |
| `model_cache_ttl` | duration | `5m`       | How long to cache upstream `/v1/models` responses. Set to `0` to disable caching |

## Backends

```yaml
backends:
  - name: openrouter # Used as model prefix
    type: openai # OpenAI-compatible API
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."
    enabled: true # Optional: disable without removing
    models: # Optional: restrict to these models
      - claude-sonnet-4
      - gpt-4o
    extra_headers: # Optional: headers sent to backend
      HTTP-Referer: "https://myapp.com"
```

| Option          | Type   | Required | Description                                                            |
| --------------- | ------ | -------- | ---------------------------------------------------------------------- |
| `name`          | string | yes      | Identifier for this backend (used as `name/model-id`)                  |
| `type`          | string | yes      | Backend type. Currently only `openai` (OpenAI-compatible) is supported |
| `base_url`      | string | yes      | Provider's API base URL                                                |
| `api_key`       | string | yes      | Your provider's API key                                                |
| `enabled`       | bool   | no       | Set to `false` to disable backend without removing from config         |
| `models`        | list   | no       | Restrict to specific models. If omitted, all model IDs are accepted    |
| `extra_headers` | map    | no       | Additional HTTP headers forwarded to the backend                       |

### Model Specification

Models can be simple strings or objects with metadata:

```yaml
models:
  - gpt-4o # Simple string
  - id: claude-opus-4 # Object form
    context_length: 200000
    max_output_tokens: 4096
```

When using object form:

- `id` — The model identifier sent to the backend
- `context_length` — Context window size (used for display)
- `max_output_tokens` — Maximum output tokens (used for display)

## Clients

Named clients with optional per-backend API key overrides:

```yaml
clients:
  - name: "vscode-work"
    api_key: "client-specific-key"
    backend_keys: # Override specific backend keys
      openrouter: "sk-or-v1-work-account"
```

| Option         | Type   | Required | Description                                           |
| -------------- | ------ | -------- | ----------------------------------------------------- |
| `name`         | string | yes      | Display name for this client                          |
| `api_key`      | string | yes      | API key this client uses to connect to proxy          |
| `backend_keys` | map    | no       | Map of backend name → API key to use for that backend |

## Routing

Configure model-specific routing with fallback:

```yaml
routing:
  models:
    - model: claude-sonnet-4
      backends: ["openrouter", "zai", "anthropic"]
    - model: "gpt-*"
      backends: ["openai"]
```

| Option     | Type   | Required | Description                                               |
| ---------- | ------ | -------- | --------------------------------------------------------- |
| `model`    | string | yes      | Model ID or wildcard pattern (e.g., `gpt-*`)              |
| `backends` | list   | yes      | Ordered list of backends to try. First available is used. |

Wildcard patterns use prefix matching: `gpt-*` matches `gpt-4o`, `gpt-3.5-turbo`, etc.

## Example Config

```yaml
server:
  listen: ":8080"
  api_keys:
    - "my-proxy-key"
  stats_path: "stats.db"

backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."

  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "..."
    models:
      - glm-5.1
      - glm-5-turbo

  - name: local
    type: openai
    base_url: http://localhost:11434/v1
    api_key: "dummy"
    enabled: false

clients:
  - name: "personal"
    api_key: "personal-key"
  - name: "work"
    api_key: "work-key"
    backend_keys:
      openrouter: "sk-or-v1-work-key"

routing:
  models:
    - model: claude-sonnet-4
      backends: ["openrouter", "zai"]
```

## Hot Reload

The config file is watched for changes. Edit and save `config.yaml` to apply changes without restarting the server.

You can also trigger a reload by sending `SIGHUP`:

```bash
kill -HUP <pid>
```

Or edit directly in the web UI at **Settings > Raw Config Editor** and click Save.
