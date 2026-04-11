# User Testing

Testing surface, tools, and resource classification for validation.

**What belongs here:** Validation surfaces, testing tools, resource costs, testing gotchas.

---

## Validation Surface

### API Surface
- **Endpoints**: `/v1/chat/completions`, `/v1/responses`, `/v1/models`, `/health`
- **Tool**: curl
- **Auth**: Bearer token from `config.yaml` (server.api_keys or clients[].api_key)
- **Setup**: Start proxy with `go run ./cmd/llmapiproxy`, ensure config.yaml has at least one API key and one backend

### Web UI Surface
- **Endpoints**: `/ui/settings`, `/ui/oauth/login/{backend}`, `/ui/oauth/callback/{backend}`, `/ui/oauth/status`
- **Tool**: agent-browser
- **Auth**: None (web UI is unauthenticated)
- **Setup**: Same proxy instance as API surface

## Validation Concurrency

- **Machine**: 32 GB RAM, 12 cores (Apple Silicon)
- **Resource per agent-browser validator**: ~300 MB browser + ~50 MB proxy = ~350 MB
- **Max concurrent validators**: 5 (well within 70% headroom of ~22 GB available)
- **API validators (curl)**: Negligible resource usage, unlimited concurrency

## Testing Notes

- No existing test infrastructure — first tests establish patterns
- Use `httptest.NewServer` for mocking upstream APIs in Go tests
- For real integration testing, the proxy needs valid OAuth credentials:
  - Copilot: Requires active GitHub Copilot subscription + local auth (gh CLI)
  - Codex: Requires ChatGPT Plus/Pro subscription + OAuth PKCE flow through browser
- Token-related tests should NOT use real API keys — use test servers

---

## Environment Observations (Round 1)

### GitHub Copilot Token Exchange
- The `gh auth token` provides a token with scopes: `gist, read:org, repo`
- The Copilot token exchange endpoint (`/copilot_internal/v2/token`) returns **HTTP 404** with this token
- **Root cause**: This endpoint is internal and requires either a Copilot-specific OAuth token or an active Copilot subscription
- **Impact**: Live Copilot API assertions (VAL-COPILOT-001 through VAL-COPILOT-023, VAL-COPILOT-026-030) cannot be tested without an active Copilot subscription
- These assertions are **blocked** pending Copilot subscription activation

### Proxy Startup
- Proxy starts successfully with copilot backend config: `go run ./cmd/llmapiproxy -config config-test-oauth.yaml`
- Config flag is `-config` (not env var)
- Models are listed correctly at `/v1/models` with `copilot/` prefix
- Health endpoint at `/health` returns `{"status":"ok"}`

### Test Config
- Created `config-test-oauth.yaml` at repo root for testing with Copilot backend only
- Uses `test-proxy-key-12345` as the API key

### Go Test Results
- `go test ./internal/oauth/...` — All tests pass (token store, discovery, exchange)
- `go test ./internal/backend/...` — All tests pass (copilot backend, registry)
- `go test ./internal/config/...` — All tests pass (config, OAuthConfig, validation)
- Tests use `httptest.NewServer` for mocking, covering all assertions with mock servers

---

## Flow Validator Guidance: API Surface (curl + Go tests)

### Isolation Rules
- Go tests are self-contained (use temp directories, mock servers)
- Live proxy tests share the same proxy instance
- Do not modify the main `config.yaml` — use `config-test-oauth.yaml`
- Each test should clean up its temp files

### Boundaries
- Do NOT send requests to external APIs (GitHub, OpenAI) from Go unit tests — use mocks
- Live proxy tests can attempt real API calls but must handle failures gracefully
- Do not create/modify files outside of temp directories and `.factory/validation/`

### Tool Setup
- Start proxy: `go run ./cmd/llmapiproxy -config config-test-oauth.yaml`
- Stop proxy: `lsof -ti :8000 | xargs kill`
- Run unit tests: `go test ./internal/oauth/... ./internal/backend/... ./internal/config/... -v -count=1`
- Curl auth header: `Authorization: Bearer test-proxy-key-12345`

---

## Flow Validator Guidance: Codex Backend Core (Go tests + curl)

### Isolation Rules
- Go tests use `httptest.NewServer` for mocking — fully isolated, no shared state
- Live proxy tests share the proxy instance on port 8000
- Do not modify the main `config.yaml`
- Use temp directories for test token files

### Boundaries
- Do NOT call external APIs (chatgpt.com, auth.openai.com) — use mock servers
- Live proxy tests can verify endpoint routing and error handling with mock tokens
- Do not modify source code — only test the existing implementation

### Assertions Covered
- VAL-CODEX-001 through VAL-CODEX-009: Core Codex backend (non-streaming, streaming, model listing, responses API, routing)
- VAL-CODEX-015 through VAL-CODEX-045: Error handling, format translation, concurrent requests, config hot-reload, health endpoint

### Tool Setup
- Run Go tests: `go test ./internal/backend/... -v -count=1 -run "Codex"`
- Run proxy handler tests: `go test ./internal/proxy/... -v -count=1`
- Build: `go build ./cmd/llmapiproxy`
- Start proxy (for live testing): `go run ./cmd/llmapiproxy -config config-test-oauth.yaml`

### Important Notes
- Mock tokens exist at `internal/backend/tokens/codex-token.json` with placeholder values
- No real Codex OAuth credentials are available — live API tests will fail
- Tests validate behavior through mock HTTP servers simulating the Codex responses API

---

## Flow Validator Guidance: Codex OAuth Flow (Go tests + browser)

### Isolation Rules
- Go tests are self-contained — use mock OAuth servers
- Browser tests need a running proxy instance
- Do not share browser sessions between validators

### Boundaries
- Do NOT initiate real OAuth flows against auth.openai.com
- Use mock OAuth servers in tests to simulate the PKCE flow
- Browser tests are limited to verifying the redirect URL structure and callback handling

### Assertions Covered
- VAL-TOKEN-020: Proactive Codex token refresh using refresh_token
- VAL-TOKEN-021: Codex refresh token rotation
- VAL-TOKEN-022: Codex refresh token expiry or revocation
- VAL-TOKEN-039: Upstream 401 triggers immediate re-authentication (Codex)
- VAL-TOKEN-041: OAuth callback CSRF protection (state parameter validation)
- VAL-TOKEN-042: PKCE code verifier never logged or persisted to disk
- VAL-TOKEN-043: OAuth callback timeout — pending state cleaned up
- VAL-CODEX-010: OAuth PKCE flow initiation from web UI
- VAL-CODEX-011: OAuth callback exchanges authorization code for tokens
- VAL-CODEX-012: OAuth callback rejects invalid or expired authorization codes
- VAL-CODEX-013: Token refresh via refresh_token when access token expires
- VAL-CODEX-014: Token refresh failure returns clear authentication error

### Tool Setup
- Run Go tests: `go test ./internal/oauth/... -v -count=1 -run "Codex|PKCE|OAuth"`
- Run backend tests: `go test ./internal/backend/... -v -count=1 -run "Codex.*OAuth|Codex.*Callback|Codex.*Login"`

### Important Notes
- No real OpenAI credentials — browser-based OAuth assertions (VAL-CODEX-010, VAL-CODEX-011) are BLOCKED
- Go tests with mock OAuth servers can validate the PKCE logic, token exchange, state validation, etc.
- VAL-CODEX-010 and VAL-CODEX-011 require agent-browser to initiate and complete a real OAuth flow — cannot be tested without real OpenAI credentials

---

## Flow Validator Guidance: OAuth Web UI Surface (agent-browser)

### Isolation Rules
- All validators share the same proxy instance on port 8000
- Browser tests interact with the same /ui/settings page
- Assertions that mutate global state (disconnect, toggle backends) must be serialized with other browser validators
- Do not run disconnect/toggle tests simultaneously with status-display tests
- Each browser session should use a unique session ID to avoid conflicts

### Boundaries
- Do NOT click Disconnect on backends that other validators need to remain connected
- Do NOT modify the proxy config file directly — use the proxy's web UI for changes
- Screenshots should be saved to the evidence directory
- After testing, leave the proxy in the same state (reconnect any disconnected backends)

### Assertions Covered
- VAL-UI-001 through VAL-UI-017: OAuth Web UI assertions
- VAL-TOKEN-032: Dashboard reflects token status
- VAL-CROSS-002, VAL-CROSS-007, VAL-CROSS-009 through VAL-CROSS-015: Cross-area browser tests
- VAL-CROSS-020: OAuth disconnect and re-auth
- VAL-CODEX-010, VAL-CODEX-011: Codex OAuth browser flow

### Tool Setup
- Proxy URL: http://localhost:8000
- Settings page: http://localhost:8000/ui/settings
- OAuth status: http://localhost:8000/ui/oauth/status
- Health: http://localhost:8000/health
- API auth: Bearer test-proxy-key-12345
- Start proxy: `go run ./cmd/llmapiproxy -config config-test-oauth-ui.yaml`
- Browser session prefix: eacf7bf789f7

### Important Notes
- The copilot backend will show "Not connected" or "Connected" depending on whether gh auth token is available and Copilot token exchange works
- The codex backend will show "Not connected" since no real OpenAI credentials are configured
- HTMX auto-refresh is set to 30 seconds — wait at least 30 seconds to observe status updates
- CSS classes use CSS variables: --green (#34d399), --amber (#fbbf24), --red (#f87171) for token states

---

## Flow Validator Guidance: Cross-Area API Surface (curl)

### Isolation Rules
- Curl tests share the same proxy instance on port 8000
- Tests that modify config (SIGHUP, config hot-reload) should be run carefully
- Config hot-reload tests should not run simultaneously with browser tests

### Boundaries
- Do NOT send requests to external APIs with real credentials — test routing and error handling only
- Config hot-reload tests modify config-test-oauth-ui.yaml — clean up after testing
- Do NOT stop the proxy from within a validator

### Assertions Covered
- VAL-CROSS-001, VAL-CROSS-003 through VAL-CROSS-006, VAL-CROSS-008: Core API cross-area tests
- VAL-CROSS-016, VAL-CROSS-017, VAL-CROSS-018, VAL-CROSS-019: Advanced API cross-area tests

### Tool Setup
- Proxy URL: http://localhost:8000
- API endpoint: http://localhost:8000/v1/chat/completions
- Auth header: Authorization: Bearer test-proxy-key-12345
- Health: http://localhost:8000/health
- Models: http://localhost:8000/v1/models
