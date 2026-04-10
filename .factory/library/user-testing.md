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
