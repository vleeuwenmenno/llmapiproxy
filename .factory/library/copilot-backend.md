# Copilot Backend Implementation

## What was implemented

- `CopilotBackend` in `internal/backend/copilot.go` implementing the `Backend` interface
- `NewCopilotBackend` constructor that accepts `BackendConfig`, `Discoverer`, `CopilotExchanger`, and `TokenStore`
- 24 tests in `internal/backend/copilot_test.go`
- Registry updated in `internal/backend/registry.go` to create real `CopilotBackend` instances (not OpenAI placeholders)

## Key patterns

- CopilotBackend gets Copilot token via `getCopilotToken()` which uses `Discoverer.DiscoverGitHubToken()` + `CopilotExchanger.GetOrRefresh()`
- Token discovery uses the priority chain: COPILOT_GITHUB_TOKEN → GH_TOKEN → GITHUB_TOKEN → gh CLI → hosts.yml → persisted file
- All Copilot-specific headers are set: Authorization, Editor-Version, Editor-Plugin-Version, Copilot-Integration-Id, User-Agent, X-Request-Id
- `APIKeyOverride` is intentionally ignored (Copilot uses local GitHub auth, not configurable API keys)
- Upstream 401 triggers token clear + single retry (loop prevention via `maxAuthRetries = 1`)
- Token files stored at `tokens/<backend-name>-token.json` by default, or custom path via `oauth.token_path` config

## Files changed

- `internal/backend/copilot.go` — New CopilotBackend implementation
- `internal/backend/copilot_test.go` — 24 tests covering all validation assertions
- `internal/backend/registry.go` — Updated `createBackend` to instantiate real CopilotBackend with token store, discoverer, and exchanger

## Assertions fulfilled

- VAL-TOKEN-038: Upstream 401 triggers re-authentication
- VAL-TOKEN-040: Re-auth retry has loop prevention (max 1 retry)
- VAL-COPILOT-001 through VAL-COPILOT-030: All Copilot backend behavior assertions
