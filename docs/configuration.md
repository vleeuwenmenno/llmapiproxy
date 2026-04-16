# Configuration Reference

Configuration is loaded from a YAML file specified with the `--config` flag (default: `data/config.yaml`). The config is hot-reloadable — edit and save the file, or send `SIGHUP`, and backends reload without restarting.

## Complete Reference

```yaml
# ── Server ──────────────────────────────────────────────────────
server:
  # Host to bind to. Empty string = all interfaces (0.0.0.0).
  # Use "localhost" or "127.0.0.1" to restrict to local access only.
  host: ""

  # Port to listen on. Defaults to 8080 if not set.
  port: 8000

  # Legacy: combined host:port (e.g. ":8000", "localhost:8080").
  # If set, it is used as a fallback when host/port are not specified.
  # The host and port fields above take precedence.
  # listen: ":8000"

  # Optional: externally-reachable domain for OAuth callbacks and UI links.
  # Set this when the proxy runs on a different machine from the user's browser,
  # e.g. behind a reverse proxy or on a Tailscale tailnet.
  # Examples:
  #   "myserver"                → http://myserver:<port>
  #   "myserver.tail:8000"      → http://myserver.tail:8000
  #   "https://example.com"     → https://example.com (scheme used verbatim)
  # domain: ""

  # API keys that clients must use to authenticate with this proxy.
  # Clients send: Authorization: Bearer <one-of-these-keys>
  api_keys:
    - "your-proxy-api-key-here"

  # Enable username/password authentication for the web UI dashboard.
  # When enabled, all /ui/* routes require a valid session cookie.
  # The proxy API (/v1/*) is NOT affected — it continues using API key auth.
  # Manage users via CLI: llmapiproxy user add <username>
  # web_auth: false

  # Path to the SQLite database storing web UI users.
  # Defaults to data/users.db when web_auth is enabled.
  # users_db_path: "data/users.db"

  # HMAC secret for signing session cookies. When empty (default), a random
  # secret is generated on startup — sessions are invalidated on restart.
  # Set this (16+ characters) to persist sessions across restarts.
  # web_auth_secret: ""

  # Path to the SQLite database used to persist request statistics.
  # Defaults to data/stats.db.
  stats_path: "data/stats.db"

  # Set to true to disable stats tracking entirely.
  # disable_stats: false

  # Path to the SQLite database used for persistent chat sessions.
  # Defaults to data/chat.db.
  # chat_db_path: "data/chat.db"

  # Model used to auto-generate chat session titles.
  # Must match a model ID available through one of your backends.
  # If not set, titles default to a truncated version of the first message.
  # title_model: "glm-5-turbo"

  # Default model when none is specified in the request.
  # default_model: "openrouter/gpt-4o"

  # How long to cache dynamically fetched model lists from upstream providers.
  # Accepts Go duration strings (e.g. "5m", "30s", "1h"). Default: "5m".
  # Set to "0s" to disable caching.
  # model_cache_ttl: "5m"

# ── Identity Spoofing ──────────────────────────────────────────
# Make the proxy's outgoing requests look like they came from a specific
# CLI tool. This sets User-Agent and custom headers on upstream requests.
#
# Built-in profiles: none, codex-cli, gemini-cli, copilot-vscode, opencode, claude-code
# Default: "none" (passthrough — no spoofing)
#
# WARNING: Identity spoofing is best-effort. It mimics CLI tool request
# signatures but cannot guarantee compatibility. Providers may still
# detect proxy usage and restrict your access.
identity_profile: "none"

# Custom identity profiles (in addition to the built-in ones).
# custom_identity_profiles:
#   - id: "my-tool"
#     display_name: "My Custom Tool"
#     user_agent: "my-tool/1.0 ({{.OS}}; {{.Arch}})"
#     headers:
#       X-Custom-Header: "my-value"

# ── Backends ───────────────────────────────────────────────────
backends:
  - name: openrouter # Required: unique identifier (used as model prefix)
    type: openai # Required: openai, anthropic, copilot, or codex
    base_url: https://openrouter.ai/api/v1 # Required: provider API base URL
    api_key: "sk-or-v1-..." # Required for openai/anthropic (not for copilot/codex)
    enabled: true # Optional: set to false to disable without removing

    # Optional: restrict to specific models. If omitted, all models are accepted.
    models:
      - claude-sonnet-4 # Simple string form
      - id: gpt-4o # Object form with metadata
        context_length: 128000
        max_output_tokens: 16384

    # Optional: additional HTTP headers forwarded to the backend.
    extra_headers:
      HTTP-Referer: "https://my-app.example.com"
      X-Title: "My App"

    # Optional: override model discovery endpoint (different from base_url).
    # Useful when the completion endpoint differs from the models endpoint.
    # models_url: "https://other-endpoint.example.com/v1"

    # Optional: models to never route through this backend.
    # These models won't appear in /v1/models or the web UI for this backend.
    # disabled_models:
    #   - some-model-id

    # Optional: per-backend identity profile override.
    # identity_profile: "opencode"

    # Optional: OAuth configuration (codex backend only).
    # oauth:
    #   client_id: "your-client-id"
    #   scopes: ["openid", "profile", "email", "offline_access"]
    #   auth_url: "https://auth.example.com/authorize"
    #   token_url: "https://auth.example.com/oauth/token"
    #   token_path: "data/tokens/"

# ── Clients ───────────────────────────────────────────────────
# Named clients each get their own API key. The name shows up in
# request analytics so you can see which app made which call.
clients:
  - name: "personal"
    api_key: "personal-proxy-key"
  - name: "work"
    api_key: "work-proxy-key"
    backend_keys: # Optional: per-backend API key overrides
      openrouter: "sk-or-v1-work-key"

  # Special client: "playground" is used by the built-in Playground web UI.
  # It is hidden from the Settings → API Keys page.
  # - name: playground
  #   api_key: "sk-proxy-internal-playground-<random>"

# ── Routing ───────────────────────────────────────────────────
# When the same model ID is served by multiple backends, the
# routing section controls how requests are distributed.
routing:
  # Global default strategy: priority, round-robin, race, staggered-race
  strategy: priority

  # Default stagger delay for staggered-race strategy (milliseconds).
  # stagger_delay_ms: 500

  # Per-model routing overrides.
  models:
    - model: glm-5.1
      strategy: round-robin # Optional: per-model strategy override
      backends:
        - zai-coding
        - zen
        - go
      # Optional: skip specific backends for this model only.
      # disabled_backends:
      #   - zai-coding
    - model: kimi-k2.5
      strategy: race
      backends:
        - zen
        - go

  # Circuit breaker: automatically suspend backends that return
  # consecutive 429 (rate limit) responses.
  circuit_breaker:
    enabled: true # Enable/disable (default: true)
    threshold: 3 # Consecutive 429s before tripping (default: 3)
    cooldown: 300 # Seconds to keep suspended (default: 300 = 5 min)
```

## Server Options

| Option            | Type     | Default               | Description                                                     |
| ----------------- | -------- | --------------------- | --------------------------------------------------------------- |
| `host`            | string   | `""` (all interfaces) | Bind host                                                       |
| `port`            | int      | `8080`                | Bind port                                                       |
| `listen`          | string   | —                     | Legacy `host:port` format (fallback when `host`/`port` not set) |
| `domain`          | string   | —                     | Externally-reachable URL for OAuth callbacks and UI links       |
| `api_keys`        | list     | required              | API keys for client authentication                              |
| `stats_path`      | string   | `data/stats.db`       | SQLite database for request stats                               |
| `disable_stats`   | bool     | `false`               | Disable stats collection                                        |
| `chat_db_path`    | string   | `data/chat.db`        | SQLite database for chat sessions                               |
| `title_model`     | string   | —                     | Model for auto-generating chat session titles                   |
| `default_model`   | string   | —                     | Default model when none specified                               |
| `model_cache_ttl` | duration | `5m`                  | Cache duration for upstream model lists                         |
| `web_auth`        | bool     | `false`               | Enable web UI username/password auth                            |
| `users_db_path`   | string   | `data/users.db`       | SQLite database for web UI users                                |
| `web_auth_secret` | string   | —                     | HMAC key for session cookies (random if unset)                  |

## Backend Options

| Option             | Type   | Required | Description                                                                                              |
| ------------------ | ------ | -------- | -------------------------------------------------------------------------------------------------------- |
| `name`             | string | yes      | Unique identifier (used as `name/model-id` prefix)                                                       |
| `type`             | string | yes      | `openai`, `anthropic`, `copilot`, or `codex`                                                             |
| `base_url`         | string | yes      | Provider API base URL                                                                                    |
| `api_key`          | string | yes\*    | Provider API key (not needed for `copilot`/`codex`)                                                      |
| `enabled`          | bool   | no       | Set to `false` to disable without removing (`true` by default)                                           |
| `models`           | list   | no       | Restrict to specific models. Accepts strings or objects with `id`, `context_length`, `max_output_tokens` |
| `extra_headers`    | map    | no       | Additional HTTP headers forwarded to the backend                                                         |
| `models_url`       | string | no       | Override URL for model discovery (if different from `base_url`)                                          |
| `disabled_models`  | list   | no       | Model IDs to exclude from routing and model listings                                                     |
| `identity_profile` | string | no       | Per-backend identity spoofing profile override                                                           |
| `oauth`            | object | no       | OAuth config (codex backend only)                                                                        |

## Client Options

| Option         | Type   | Required | Description                              |
| -------------- | ------ | -------- | ---------------------------------------- |
| `name`         | string | yes      | Display name (appears in analytics)      |
| `api_key`      | string | yes      | Key this client uses to connect to proxy |
| `backend_keys` | map    | no       | Map of backend name → API key override   |

## Routing Options

| Option                      | Type   | Default    | Description                                                         |
| --------------------------- | ------ | ---------- | ------------------------------------------------------------------- |
| `strategy`                  | string | `priority` | Global default: `priority`, `round-robin`, `race`, `staggered-race` |
| `stagger_delay_ms`          | int    | `500`      | Default delay between staggered-race launches                       |
| `models`                    | list   | —          | Per-model routing overrides                                         |
| `circuit_breaker.enabled`   | bool   | `true`     | Enable circuit breaker                                              |
| `circuit_breaker.threshold` | int    | `3`        | Consecutive 429s before tripping                                    |
| `circuit_breaker.cooldown`  | int    | `300`      | Seconds to keep a tripped backend suspended                         |

## Hot Reload

The config file is watched for changes. Edit and save your config to apply changes without restarting the server. You can also trigger a reload by sending `SIGHUP`:

```bash
kill -HUP <pid>
```

Or edit directly in the web UI at **Settings → Raw Config Editor** and click Save.

OAuth tokens and model caches are preserved across reloads.
