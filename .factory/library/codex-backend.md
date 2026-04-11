# Codex Backend Implementation

## Overview
`internal/backend/codex.go` implements the `Backend` interface for OpenAI Codex, translating between OpenAI ChatCompletion format and the Codex Responses API format.

## Key Architecture Decisions

### Format Translation
- **ChatCompletion → Responses API**: Messages array is translated to the Responses API `input` format. System/developer messages are extracted as `instructions`. Other messages become `input` entries.
- **Responses API → ChatCompletion**: Output items with type "message" and role "assistant" are extracted. Content parts of type "output_text" are joined into the response content.
- **Streaming Translation**: The `codexStreamReader` reads Codex SSE events (e.g., `response.output_text.delta`, `response.completed`) and translates them to OpenAI ChatCompletion SSE chunks in real time.

### Endpoint
Requests are sent to `{baseURL}/responses` where baseURL defaults to `https://chatgpt.com/backend-api/codex`.

### Authentication
Uses `oauth.CodexOAuthHandler` for token management:
- Gets tokens from `oauth.TokenStore`
- Refreshes tokens via `RefreshWithRetry` when expired
- On upstream 401, refreshes token and retries once (loop prevention)

### Registry Wiring
`newCodexBackendFromConfig` in `registry.go` creates the CodexBackend with:
- Per-backend token file at `tokens/{name}-token.json`
- OAuth config from `BackendConfig.OAuth` (falls back to defaults)
- `oauth.CodexOAuthHandler` for PKCE flow and token refresh

## Testing
27 tests in `internal/backend/codex_test.go` using `httptest.NewServer` to mock:
- Codex upstream (Responses API)
- OAuth token server
- Various error scenarios (429, 404, 402, 500, 401)

## Config Hot-Reload (SIGHUP) with Token Preservation

The registry (`internal/backend/registry.go`) preserves token stores across config hot-reloads:
- Token stores are tracked separately from backends in `tokenStores` map
- On `LoadFromConfig`, existing token stores are reused for backends that persist across reloads
- Removed backends have their token stores cleaned up from the registry map (but files remain on disk)
- This ensures in-memory tokens are not lost when backends are re-created

## OAuth Status Interfaces

New optional interfaces in `internal/backend/backend.go`:
- `OAuthStatusProvider` — exposes auth status (Authenticated, NeedsReauth, TokenExpiry, etc.)
- `OAuthLoginHandler` — initiates OAuth login (used by Codex PKCE flow)
- `OAuthCallbackHandler` — handles OAuth callback code exchange
- `OAuthDisconnectHandler` — clears stored tokens

Both `CodexBackend` and `CopilotBackend` implement `OAuthStatusProvider` and `OAuthDisconnectHandler`.
`CodexBackend` additionally implements `OAuthLoginHandler` and `OAuthCallbackHandler`.

## Web UI OAuth Endpoints

Routes registered in `cmd/llmapiproxy/main.go`:
- `GET /ui/oauth/status` — HTMX fragment showing auth status for all OAuth backends
- `GET /ui/oauth/login/{backend}` — initiates OAuth flow (redirect to provider)
- `GET /ui/oauth/callback/{backend}` — handles OAuth callback
- `POST /ui/oauth/disconnect/{backend}` — clears stored tokens

Template: `internal/web/templates/oauth_status.html` — HTMX auto-refresh every 30s

## Health Endpoint

`GET /health` now returns:
```json
{
  "status": "ok" | "degraded",
  "backends": [
    {
      "backend_name": "codex",
      "backend_type": "codex",
      "authenticated": true,
      "token_expiry": "2026-04-11T12:00:00Z",
      "token_source": "codex_oauth",
      "needs_reauth": false
    }
  ]
}
```
Status is "degraded" if any OAuth backend needs re-authentication.
