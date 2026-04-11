# OAuth Config and Registry

## What was implemented

- `OAuthConfig` struct in `internal/config/config.go` with fields: `client_id`, `scopes`, `token_path`, `auth_url`, `token_url`
- `BackendConfig.OAuth` field (optional `*OAuthConfig`)
- `BackendConfig.IsOAuthBackend()` method — returns true for `copilot` and `codex` types
- Config validation updated: `api_key` not required for OAuth backends (`copilot`, `codex`)
- `Registry.LoadFromConfig()` type switch handles `openai`, `copilot`, `codex`, and unknown types
- `Registry.Get()`, `Registry.Has()`, `Registry.Names()` helper methods added
- Config hot-reload via SIGHUP works for OAuth backends (add/remove/update)
- `config.example.yaml` updated with commented-out copilot and codex examples

## Key patterns

- OAuth backends are identified by `type: copilot` or `type: codex` in config
- The `api_key` field is optional for OAuth backends but `base_url` is still required
- Copilot/codex backends currently use OpenAI backend as a placeholder until the actual implementations are built
- Unknown backend types are logged as warnings and skipped (not fatal)
- Hot-reload replaces the entire backend map on each config reload

## Files changed

- `internal/config/config.go` — OAuthConfig struct, BackendConfig.OAuth, IsOAuthBackend(), validation
- `internal/config/config_test.go` — 22 tests covering OAuthConfig, validation, YAML parsing, hot-reload
- `internal/backend/registry.go` — type switch in LoadFromConfig(), Get/Has/Names helpers
- `internal/backend/registry_test.go` — 15 tests covering type switch, hot-reload, helper methods
- `config.example.yaml` — commented-out copilot and codex examples

## Assertions fulfilled

- VAL-TOKEN-027: Config hot-reload adds new OAuth backend
- VAL-TOKEN-028: Config hot-reload removes OAuth backend
- VAL-TOKEN-029: Config hot-reload updates OAuth backend configuration
- VAL-TOKEN-031: Multiple OAuth backends operate independently
- VAL-COPILOT-024: Copilot backend config validates required fields (base_url still required)
- VAL-COPILOT-025: Copilot backend config does not require api_key field
