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
