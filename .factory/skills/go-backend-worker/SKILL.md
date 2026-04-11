---
name: go-backend-worker
description: Implements Go backend features — new packages, types, HTTP handlers, config changes, and tests. Validates with go test, curl, and agent-browser.
---

# Go Backend Worker

NOTE: Startup and cleanup are handled by `worker-base`. This skill defines the WORK PROCEDURE.

## When to Use This Skill

Use this worker type for all implementation features in this mission:
- New Go packages (internal/oauth/, internal/backend/copilot.go, codex.go)
- Config changes (OAuthConfig, validation, registry)
- HTTP handler additions (responses API, OAuth endpoints)
- Web UI changes (templates, OAuth handlers)
- Cross-area integration testing and fixes

## Required Skills

- `agent-browser` — For verifying web UI changes (OAuth dashboard, settings page). Use when the feature involves /ui/ routes or templates.

## Work Procedure

### 1. Read Mission Context

Read these files for full context:
- `mission.md` — Mission objectives and plan
- `AGENTS.md` — Coding conventions, boundaries, architecture overview
- `.factory/library/architecture.md` — System architecture
- `.factory/services.yaml` — Build/test commands and service definitions
- `validation-contract.md` — The assertions this feature must fulfill

Read the feature description carefully. Understand what's being built and which assertions it fulfills.

### 2. Explore Existing Code

Before writing any code:
- Read the relevant existing source files that the feature touches
- Understand the current patterns (e.g., how OpenAIBackend works, how config.Manager works)
- Check for any existing tests in the same packages
- Read the feature's `preconditions` to understand dependencies

### 3. Write Tests First (Red)

For every new behavior:
1. Write Go tests (`_test.go` files) BEFORE implementation
2. Use standard `testing` package — no external frameworks
3. Use `httptest.NewServer` for HTTP-related tests (mock upstream APIs)
4. Test file goes in the same package as the code being tested
5. Run tests to confirm they FAIL: `go test ./internal/oauth/... -run TestFeatureName -v`
6. Tests must cover:
   - Happy path behavior
   - Error handling
   - Edge cases (empty inputs, missing fields, network failures)
   - Thread safety where applicable (concurrent goroutines with `sync.WaitGroup`)

### 4. Implement (Green)

Write the implementation to make tests pass:
1. Follow Go coding conventions from AGENTS.md
2. Match existing code patterns in the codebase
3. Use `fmt.Errorf("...: %w", err)` for error wrapping
4. Ensure thread safety with `sync.RWMutex` where needed
5. No secrets in logs — mask tokens in log output
6. Token files must have 0600 permissions

### 5. Update Wiring (if needed)

If the feature adds new packages, types, or routes:
- Update `cmd/llmapiproxy/main.go` to wire new components
- Update `internal/backend/registry.go` if adding new backend types
- Update `internal/config/config.go` if adding new config fields
- Update `config.example.yaml` with new example entries
- Register new HTTP routes in the appropriate router

### 6. Build and Test

Run the full verification suite:
```bash
go build ./cmd/llmapiproxy          # Must succeed
go vet ./...                         # Must pass
go test ./...                        # All tests pass
```

If any existing tests break, fix them before proceeding.

### 7. Manual Verification with curl

For API-related features, start the proxy and test with curl:
```bash
# Copy config.example.yaml to config.yaml if needed, add OAuth backend config
cp config.example.yaml config.yaml
# Edit config.yaml to add the OAuth backend for testing
go run ./cmd/llmapiproxy &
# Test the endpoint
curl -s http://localhost:8000/v1/models | jq .
curl -s -X POST http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer <test-key>" \
  -H "Content-Type: application/json" \
  -d '{"model":"copilot/gpt-4o","messages":[{"role":"user","content":"hello"}]}'
# Clean up
kill %1
```

Document the exact curl commands and responses in the handoff.

### 8. UI Verification with agent-browser (if applicable)

If the feature involves web UI changes:
1. Start the proxy: `go run ./cmd/llmapiproxy`
2. Use `agent-browser` skill to:
   - Navigate to `http://localhost:8000/ui/settings`
   - Verify OAuth Connections section is visible
   - Check token status indicators
   - Test Connect/Disconnect buttons
3. Capture screenshots as evidence
4. Stop the proxy cleanly

### 9. Final Checks

- Run `go vet ./...` — must pass
- Run `go test ./...` — all tests pass
- Verify no secrets in committed files
- Verify existing functionality still works (dashboard, playground, settings)

## Example Handoff

```json
{
  "salientSummary": "Implemented CopilotBackend in internal/backend/copilot.go with GitHub token discovery, Copilot token exchange, and OpenAI-compatible API forwarding. Added 12 tests covering token exchange, streaming, error handling, and concurrent requests. Verified with curl against running proxy — got successful chat completion response from Copilot API.",
  "whatWasImplemented": "New file internal/backend/copilot.go implementing Backend interface for GitHub Copilot. Discovers GitHub token from env vars/gh CLI/hosts.yml, exchanges for Copilot token via api.github.com/copilot_internal/v2/token, forwards requests to api.githubcopilot.com/chat/completions with required headers. Supports Individual/Business/Enterprise base URLs. Updated registry.go with type switch for copilot backend. Updated config.go with OAuthConfig struct. Added 12 tests in internal/backend/copilot_test.go.",
  "whatWasLeftUndone": "Did not test with Business/Enterprise base URLs (requires different subscription tiers). Stats recording for streaming requests needs verification with longer conversations.",
  "verification": {
    "commandsRun": [
      {"command": "go build ./cmd/llmapiproxy", "exitCode": 0, "observation": "Build succeeded"},
      {"command": "go vet ./...", "exitCode": 0, "observation": "No issues"},
      {"command": "go test ./internal/backend/... -v", "exitCode": 0, "observation": "12 tests passing"},
      {"command": "go test ./internal/oauth/... -v", "exitCode": 0, "observation": "8 tests passing"},
      {"command": "curl -s http://localhost:8000/v1/models -H 'Authorization: Bearer test-key'", "exitCode": 0, "observation": "Returns model list including copilot/gpt-4o"},
      {"command": "curl -s -X POST http://localhost:8000/v1/chat/completions -H 'Authorization: Bearer test-key' -H 'Content-Type: application/json' -d '{\"model\":\"copilot/gpt-4o\",\"messages\":[{\"role\":\"user\",\"content\":\"Say hello\"}]}'", "exitCode": 0, "observation": "Returns chat completion response from Copilot API"}
    ],
    "interactiveChecks": [
      {"action": "Navigated to http://localhost:8000/ui/settings", "observed": "Settings page loads, OAuth section not yet visible (expected — UI feature is milestone 3)"}
    ]
  },
  "tests": {
    "added": [
      {"file": "internal/backend/copilot_test.go", "cases": [
        {"name": "TestCopilotBackend_ChatCompletion", "verifies": "Non-streaming chat completion through Copilot"},
        {"name": "TestCopilotBackend_ChatCompletionStream", "verifies": "Streaming SSE chat completion"},
        {"name": "TestCopilotBackend_ListModels", "verifies": "Model listing from Copilot"},
        {"name": "TestCopilotBackend_SupportsModel", "verifies": "Model matching with prefix"},
        {"name": "TestCopilotBackend_RequiredHeaders", "verifies": "All Copilot-specific headers sent correctly"},
        {"name": "TestCopilotBackend_NoGitHubToken", "verifies": "Error when no token available"},
        {"name": "TestCopilotBackend_TokenRefresh", "verifies": "Token refresh on expiry"},
        {"name": "TestCopilotBackend_Upstream401", "verifies": "Re-auth on upstream 401"},
        {"name": "TestCopilotBackend_RateLimit", "verifies": "Rate limit error forwarding"},
        {"name": "TestCopilotBackend_ConcurrentRequests", "verifies": "Thread safety under load"},
        {"name": "TestCopilotBackend_BusinessURL", "verifies": "Business base URL routing"},
        {"name": "TestCopilotBackend_EnterpriseURL", "verifies": "Enterprise base URL routing"}
      ]},
      {"file": "internal/oauth/token_store_test.go", "cases": [
        {"name": "TestTokenStore_SaveAndLoad", "verifies": "Token persistence and loading"},
        {"name": "TestTokenStore_FilePermissions", "verifies": "0600 permissions on token files"},
        {"name": "TestTokenStore_ConcurrentAccess", "verifies": "Thread-safe concurrent reads/writes"}
      ]}
    ]
  },
  "discoveredIssues": []
}
```

## When to Return to Orchestrator

- Feature depends on a type, interface, or function that doesn't exist yet and should be in another feature's scope
- Requirements are ambiguous or contradictory (check validation-contract.md first)
- Existing bugs in the codebase block this feature (describe the bug and how it blocks)
- Cannot complete the feature within the mission boundaries (ports, off-limits directories, etc.)
- External API is unreachable and cannot be mocked for testing (describe what was tried)
