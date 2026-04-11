# Copilot Backend Implementation

## What was implemented

- `CopilotBackend` in `internal/backend/copilot.go` implementing the `Backend` interface
- `NewCopilotBackend` constructor that accepts `BackendConfig`, `DeviceCodeHandler`, and `TokenStore`
- `DeviceCodeHandler` in `internal/oauth/device_code.go` implementing the GitHub Device Code Flow
- `device_code.html` template for the web UI showing user_code and verification URL
- Tests in `internal/backend/copilot_test.go` and `internal/oauth/device_code_test.go`

## Key patterns

- **Authentication**: CopilotBackend uses GitHub Device Code Flow instead of local token discovery.
  - User initiates flow via web UI (click "Connect" on settings page)
  - POST github.com/login/device/code → user_code + verification_uri
  - User visits URL, enters code, authorizes
  - Background polling: POST github.com/login/oauth/access_token → GitHub access token
  - Exchange: GitHub token → GET api.github.com/copilot_internal/v2/token → Copilot API token
  - Both tokens stored in TokenStore (GitHub token for on-demand re-validation)

- **Token lifecycle**: Token is long-lived, validated on-demand (no proactive refresh).
  - `GetCopilotToken()` checks for valid cached token
  - If expired, re-exchanges stored GitHub token for fresh Copilot token
  - If no GitHub token stored (e.g., after disconnect), user must re-initiate device code flow

- **On-demand re-validation**: When the Copilot token expires:
  1. Check if GitHub token is stored alongside Copilot token
  2. If yes, re-exchange GitHub token for fresh Copilot token
  3. If no, return error prompting user to re-authenticate via web UI

- **OAuthLoginHandler**: CopilotBackend implements `OAuthLoginHandler` via `InitiateLogin()`.
  - Returns JSON-encoded `DeviceCodeLoginInfo` instead of a redirect URL
  - Starts background polling goroutine
  - Web handler detects device code flow vs redirect flow by checking for UserCode in response

- **401 retry**: Upstream 401 triggers `forceExpireToken()` which preserves the GitHub token
  while marking the Copilot token as expired. The next `GetCopilotToken()` call re-exchanges.

- **No dependency on gh CLI or env vars**: All authentication goes through device code flow.

## Files changed

- `internal/oauth/device_code.go` — New DeviceCodeHandler implementation
- `internal/oauth/device_code_test.go` — 16 tests for device code flow
- `internal/oauth/token_store.go` — Added GitHubToken field to TokenData
- `internal/backend/copilot.go` — Replaced Discoverer/Exchanger with DeviceCodeHandler
- `internal/backend/copilot_test.go` — Updated tests for device code flow pattern
- `internal/backend/registry.go` — Updated createCopilotBackend to use DeviceCodeHandler
- `internal/web/web.go` — Updated OAuthLogin to handle device code flow
- `internal/web/templates/device_code.html` — New template for device code UI
- `internal/proxy/handler_test.go` — Updated to use new CopilotBackend constructor
- `internal/proxy/cross_area_test.go` — Updated to use new CopilotBackend constructor
- `config.example.yaml` — Updated Copilot section for device code flow

## GitHub Device Code Flow Client ID

Default: `Iv1.b507a08c87ecfe98` (GitHub Copilot VS Code extension)
This can be overridden via `oauth.client_id` in the backend config.

## Important notes

- The `token_discovery.go` file still exists but is no longer used by CopilotBackend.
  It may be removed in a future cleanup feature.
- After disconnect, users must re-initiate the device code flow (no automatic re-auth).
- The device code background polling starts when `InitiateLogin()` is called and runs
  until the user authorizes or the code expires.
