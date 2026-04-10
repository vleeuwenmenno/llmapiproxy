# Architecture

How the LLM API Proxy works — components, relationships, data flows.

## What belongs here
High-level system description: components, how they relate, data flows, invariants.
**What does NOT belong here:** Service ports/commands (use .factory/services.yaml), env vars (.factory/library/environment.md).

---

## System Overview

LLM API Proxy is a Go reverse proxy that unifies multiple LLM provider APIs behind a single OpenAI-compatible endpoint. It includes a web dashboard, request stats tracking, quota monitoring, and a chat playground.

## Core Components

### Config Manager (`internal/config/`)
- Thread-safe config holder with `sync.RWMutex`
- Hot-reload via SIGHUP or web UI
- Validates: at least one API key/client, at least one enabled backend
- `OnChange` callbacks for reactive updates (e.g., recreating backends)

### Backend Registry (`internal/backend/`)
- Maps model prefixes to Backend implementations
- `ResolveRoute(model, routing)` returns ordered `[]RouteEntry` for failover
- Currently only `OpenAIBackend` — being extended with `CopilotBackend` and `CodexBackend`

### Proxy Handler (`internal/proxy/`)
- Chi router with auth middleware on `/v1/*` routes
- Auth: Bearer token → `LookupClient()` → `*ClientConfig` in context
- Request flow: parse body → resolve route → iterate backends with failover → record stats
- Streaming: line-by-line SSE proxying with model field rewriting

### Web UI (`internal/web/`)
- Server-rendered Go templates + HTMX
- No frontend build step
- Dashboard, settings, playground, models, request detail views
- Currently NO authentication on `/ui/*` routes

### Stats Pipeline (`internal/stats/`)
- In-memory Collector with async batching
- SQLite persistent Store
- Records request counts, token usage, latency

## New Components (OAuth Mission)

### OAuth Package (`internal/oauth/`)
- **TokenStore**: Thread-safe JSON file persistence with in-memory cache
- **GitHub Token Discovery**: Priority chain of env vars → gh CLI → hosts.yml → file
- **Copilot Token Exchange**: GitHub token → copilot_internal/v2/token → Copilot API token
- **Codex OAuth PKCE**: Full OAuth 2.0 PKCE flow with CSRF state validation

### Copilot Backend (`internal/backend/copilot.go`)
- Implements `Backend` interface
- Discovers local GitHub auth, exchanges for Copilot token
- Forwards to api.githubcopilot.com with required headers
- Auto-refresh based on `refresh_in` field
- Upstream 401 triggers re-auth with single retry

### Codex Backend (`internal/backend/codex.go`)
- Implements `Backend` interface
- OAuth PKCE flow for authentication
- Forwards to chatgpt.com/backend-api/codex/responses
- Internal ChatCompletion ↔ Responses API format translation
- Also supports native `/v1/responses` passthrough

### OAuth Web Handler (`internal/web/oauth_handler.go`)
- Login initiation, callback handling, status display
- HTMX fragments for live token status

## Data Flows

### Request Flow (existing)
```
Client → AuthMiddleware → Handler → Registry.ResolveRoute → Backend.ChatCompletion → Upstream API
                                                                                          ↓
                                                                                     Stats Collector → SQLite
```

### Request Flow (OAuth backend)
```
Client → AuthMiddleware → Handler → Registry.ResolveRoute → CopilotBackend.ChatCompletion
                                                              ↓
                                                        TokenStore.ValidToken()
                                                              ↓
                                                     [cached token or refresh]
                                                              ↓
                                                     GET api.githubcopilot.com/chat/completions
```

### Copilot Token Lifecycle
```
1. Discovery: env vars → gh CLI → hosts.yml → file
2. Exchange: GitHub token → copilot_internal/v2/token → Copilot token
3. Cache: In-memory + JSON file
4. Refresh: Proactive, based on refresh_in field
5. Retry: Upstream 401 → refresh → retry once
```

### Codex OAuth Lifecycle
```
1. Initiate: PKCE code_verifier/challenge → redirect to auth.openai.com
2. Callback: code + code_verifier → auth.openai.com/oauth/token → access + refresh tokens
3. Cache: In-memory + JSON file
4. Refresh: refresh_token → new access + refresh tokens (rotation)
5. Retry: Upstream 401 → refresh → retry once
```

## Key Invariants

1. Existing OpenAI backend functionality MUST NOT be affected by OAuth changes
2. `api_key` is NOT required for OAuth backends (copilot, codex)
3. Token files MUST have 0600 permissions
4. Token refresh must be thread-safe (single refresh in flight)
5. Config hot-reload MUST work with OAuth backends
6. No secrets in logs
