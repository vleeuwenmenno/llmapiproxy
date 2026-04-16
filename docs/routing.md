# Routing & Failover

This document explains how LLM API Proxy routes incoming requests to the correct backend(s), available strategies, and how failover works.

## Overview

Every chat completion request includes a `model` field. The proxy uses this field to determine which backend(s) should handle the request. The routing system has three resolution layers, tried in order:

1. **Explicit routing config** — exact model match in `routing.models[]`
2. **Prefix routing** — `backend/model` format (e.g., `openrouter/gpt-4o`)
3. **Wildcard matching** — first backend whose `SupportsModel()` returns true

Once backends are selected, a **routing strategy** determines how they are invoked.

## Route Resolution

### 1. Explicit Config Match (Highest Priority)

If the requested model exactly matches an entry in `routing.models[]`, the configured backends are used:

```yaml
routing:
  strategy: priority
  stagger_delay_ms: 500

  models:
    - model: "gpt-4o"
      backends: ["openai", "openrouter"]
      strategy: "priority" # Optional per-model override

    - model: "claude-sonnet-4"
      backends: ["anthropic", "openrouter"]
      strategy: "race"

    - model: "glm-5.1"
      backends: ["zai-coding", "zen", "go"]
      strategy: "round-robin"
      disabled_backends: # Optional: skip specific backends
        - zai-coding
```

**Strategy resolution order:**

1. Per-model `strategy` field (if set)
2. Global `routing.strategy` field (if set)
3. Default: `"priority"`

**Stagger delay resolution order:**

1. Per-model `stagger_delay_ms` (if > 0)
2. Global `routing.stagger_delay_ms` (if > 0)
3. Default: 500ms

**Unregistered backends** in the config are silently skipped (e.g., if a backend was removed or disabled).

### 2. Prefix Routing

If no explicit config match is found, the proxy checks whether the model ID contains a `/` separator:

| Request Model               | Backend      | ModelID Forwarded |
| --------------------------- | ------------ | ----------------- |
| `openrouter/gpt-4o`         | `openrouter` | `gpt-4o`          |
| `anthropic/claude-sonnet-4` | `anthropic`  | `claude-sonnet-4` |
| `zai/glm-4-plus`            | `zai`        | `glm-4-plus`      |

The backend name is everything before the first `/`. The model ID forwarded to the backend is everything after. Prefix routing does **not** check `SupportsModel()` — the request is forwarded directly.

### 3. Wildcard Matching (Last Resort)

The proxy iterates over all registered backends and calls `SupportsModel(modelID)` on each. The first backend that returns `true` is selected.

> **Important:** Go map iteration order is non-deterministic. If multiple backends support the same model, which one is chosen is undefined. For deterministic behavior with overlapping models, always use explicit routing config.

If no backend supports the model, the proxy returns: `"no backend found for model <modelID>"`.

## Routing Strategies

| Strategy             | Behavior                                                                         | Best For                                   |
| -------------------- | -------------------------------------------------------------------------------- | ------------------------------------------ |
| **`priority`**       | Try backends sequentially. First success wins. Fallback on retryable errors.     | Preferred provider with fallback chain     |
| **`round-robin`**    | Rotate which backend leads on each request. Others remain in order for fallback. | Load balancing across equivalent providers |
| **`race`**           | Launch ALL backends concurrently. First success wins, others are cancelled.      | Lowest latency when cost is acceptable     |
| **`staggered-race`** | Launch backends with a configurable delay between each. First success wins.      | Balance between latency and cost           |

### Priority (Default)

Try backends in list order. If the first returns a retryable error, try the next:

```
Request → Backend A → 500 error → Backend B → 200 OK → Return response
                                         Backend C (never called)
```

### Round-Robin

Identical to priority for a single request, but the **starting backend rotates** across requests:

| Request # | Backend Order | Notes             |
| --------- | ------------- | ----------------- |
| 1         | A → B → C     | A leads           |
| 2         | B → C → A     | B leads (rotated) |
| 3         | C → A → B     | C leads (rotated) |
| 4         | A → B → C     | Back to A         |

Rotation uses per-model atomic counters, so concurrent requests each get a distinct slot without locks.

### Race

All backends are launched concurrently. The first successful response is returned; all other in-flight requests are cancelled via context cancellation. If ALL backends fail, the last error is returned as a 502.

```
Request → Launch A, B, C simultaneously
          B responds first (200 OK)
          Cancel A and C
          Return B's response
```

### Staggered-Race

Like race, but backends are launched with a configurable delay between each start:

```
t=0ms:    Launch Backend A
t=500ms:  Launch Backend B
t=1000ms: Launch Backend C

A responds at t=800ms → Cancel B and C → Return A's response
```

The delay is configurable per-model or globally (default: 500ms). This reduces cost compared to full race while still providing low latency if the primary backend is fast.

## Error Handling & Fallback

When a backend returns an error during **priority** or **round-robin** execution, the decision to retry depends on the error type:

| Error Type    | HTTP Status   | Action         | Rationale                               |
| ------------- | ------------- | -------------- | --------------------------------------- |
| Server error  | 5xx           | **Retry next** | Backend is broken, try another          |
| Rate limit    | 429           | **Retry next** | Backend is throttled, try another       |
| Network error | (none)        | **Retry next** | Backend unreachable, try another        |
| Client error  | 4xx (not 429) | **Stop**       | Request is invalid; retrying won't help |

### OAuth Backend Auto-Retry

Copilot and Codex backends have built-in retry logic for 401 Unauthorized:

1. The current token is invalidated
2. A fresh token is obtained (via OAuth refresh)
3. The request is retried once with the new token
4. If it fails again, the error propagates to the failover logic

This happens transparently inside the backend before the handler's failover logic sees the error.

## Model Discovery

Each backend type implements `SupportsModel()` differently:

### OpenAI / Anthropic Backends

- **Static model list configured:** Only models in the list are accepted. Wildcards are supported (e.g., `gpt-4/*` matches `gpt-4/turbo`).
- **No static list:** Models are fetched from the upstream `/v1/models` endpoint and cached with a configurable TTL (default: 5 minutes).
- **Stale-while-error:** If the upstream fetch fails but a cached list exists, the stale cache is used.

### Codex / Copilot Backends

- **No inline fetch:** `SupportsModel()` only checks existing cache — it does not trigger upstream fetches.
- **Cache warmed at startup:** Model caches are populated in parallel background goroutines when the server starts.
- **Cold cache = false:** If the cache hasn't been populated yet, `SupportsModel()` returns `false` to prevent routing to unready backends.

### Cache Configuration

```yaml
server:
  model_cache_ttl: "5m" # Default: 5 minutes. Set to "0s" to disable.
```

Cache is cleared on config reload. You can also manually refresh from the Models page in the web UI.

## Configuration Examples

### Basic Priority Failover

```yaml
backends:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: sk-...

  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: sk-or-...

routing:
  strategy: priority
```

### Race for Lowest Latency

```yaml
routing:
  strategy: race
  models:
    - model: claude-sonnet-4
      backends: ["anthropic", "openrouter"]
      strategy: race
```

### Round-Robin Load Balancing

```yaml
routing:
  strategy: round-robin
  models:
    - model: glm-5.1
      strategy: round-robin
      backends: ["zai-coding", "zen", "go"]
```

### Staggered-Race with Custom Delay

```yaml
routing:
  strategy: priority
  stagger_delay_ms: 1000
  models:
    - model: gpt-4o
      backends: ["openai", "openrouter", "copilot"]
      strategy: staggered-race
      stagger_delay_ms: 750
```
