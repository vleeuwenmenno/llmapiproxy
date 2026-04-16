# Features

## Dashboard

Open [http://localhost:8080/ui/](http://localhost:8080/ui/) for the web dashboard.

### Pages

| Page           | Path             | Description                                            |
| -------------- | ---------------- | ------------------------------------------------------ |
| **Dashboard**  | `/ui/`           | Live request feed, token totals, per-backend breakdown |
| **Models**     | `/ui/models`     | Browse models, backend toggling, routing config        |
| **Playground** | `/ui/playground` | Interactive chat for testing models                    |
| **Analytics**  | `/ui/analytics`  | Time-series charts, filtering by date/backend/model    |
| **Settings**   | `/ui/settings`   | API keys, config editor, stats controls                |

Auto-refreshes every 10 seconds.

## Stats & Analytics

Every request is tracked:

- Timestamp, backend, model
- Prompt/completion/total tokens
- Latency, status code
- Error messages (if any)
- Client name (if using client system)

Data persists to SQLite across restarts.

### Analytics Views

- **Summary**: Total requests, tokens, errors, average latency
- **Percentiles**: P50, P90, P99 latency
- **Time Series**: Token usage over time (configurable buckets)
- **Rankings**: Top backends, models, clients

### Filters

Filter analytics by:

- Time window (1h, 6h, 24h, 7d, 30d)
- Backend
- Model
- Client
- Status (success/error)

## Client System

Create named clients with separate API keys and per-backend key overrides.

Use case: Separate personal and work accounts, each with their own OpenRouter key.

```yaml
clients:
  - name: "personal"
    api_key: "personal-proxy-key"

  - name: "work"
    api_key: "work-proxy-key"
    backend_keys:
      openrouter: "sk-or-v1-work-key"
```

The client name appears in analytics for usage tracking.

## Model Routing

Configure failover routing: try backend A first, fall back to B if unavailable.

```yaml
routing:
  models:
    - model: claude-sonnet-4
      backends: ["openrouter", "zai"]
```

Wildcards are supported:

```yaml
routing:
  models:
    - model: "gpt-*"
      backends: ["openai"]
    - model: "claude-*"
      backends: ["openrouter", "zai"]
```

If the first backend is disabled or returns an error, the next is tried.

## Playground

Test models interactively from the browser:

1. Go to **Playground**
2. Select a model from the dropdown
3. Chat with streaming responses
4. View token usage and latency for each request

## Model Caching

Each backend caches its upstream `/v1/models` response to avoid spamming provider APIs on every model list request.

### Configuration

Set `model_cache_ttl` in the `server` section of your config (default: `5m`):

```yaml
server:
  model_cache_ttl: "10m" # Cache for 10 minutes
```

Set to `"0"` or `0` to disable caching — every `ListModels` call will hit upstream.

### Behavior

- **Cache hit**: Returns cached model list without contacting upstream
- **Cache miss**: Fetches from upstream, stores result with expiry
- **Stale-while-error**: If upstream fails after cache expiry, stale data is returned if available
- **Manual refresh**: Click the refresh button on the Models page to clear cache and fetch fresh data
- **Config reload**: Cache resets when config is reloaded (backends are recreated)

The TTL can also be changed from the **Settings** page without restarting.

## Backend Management

Enable or disable backends without editing config:

1. Go to **Models** page
2. Toggle the switch on any backend card
3. Click "Save" to apply

Disabled backends are skipped in routing and won't appear in model listings.

## Config Editor

Edit `config.yaml` directly from the web UI:

1. Go to **Settings**
2. Scroll to "Raw Config Editor"
3. Make changes with YAML validation
4. Click Save — backends reload automatically

## Model Metadata

The proxy includes a database of 100+ models with metadata:

- Display names
- Context lengths
- Max output tokens
- Vision capability

When multiple backends have the same model, metadata is merged (largest context window, union of capabilities).

## API Features

- **Streaming**: Full SSE support for real-time responses
- **Tool calling**: Forwarded transparently to backends
- **Model listing**: Aggregated from all backends with deduplication
- **Health check**: `GET /health` returns `{"status":"ok"}`
