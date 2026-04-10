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
