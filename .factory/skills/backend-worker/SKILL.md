---
name: backend-worker
description: Builds Go backend features — data stores, API handlers, route registration
---

# Backend Worker

NOTE: Startup and cleanup are handled by `worker-base`. This skill defines the WORK PROCEDURE.

## When to Use This Skill

Features that involve:
- Creating new Go packages (e.g., `internal/chatv2/`)
- Implementing SQLite store methods (CRUD, search, queries)
- Building HTTP handlers for API routes
- Registering Chi routes in `cmd/llmapiproxy/serve.go`
- Writing Go unit tests

## Required Skills

None — this worker writes Go code and tests only.

## Work Procedure

1. **Read context files first**: Read `mission.md`, `AGENTS.md`, `.factory/library/architecture.md`, and `.factory/library/environment.md` for architectural guidance and constraints.

2. **Write tests first (TDD)**: For every new store method or handler, write a failing test before implementation. Tests go in the same package as the code (`*_test.go`). Use the standard Go `testing` package — no external test frameworks.

3. **Implement to make tests pass**: Write the minimum code to make tests pass. Follow existing patterns from `internal/chat/store.go` for store implementations and `internal/web/web.go` for handlers.

4. **Register routes**: When adding API routes, register them in `cmd/llmapiproxy/serve.go` following the existing pattern (Chi router with `r.Get()`, `r.Post()`, etc.).

5. **Run tests and vet**: Execute `go test ./internal/chatv2/...` and `go vet ./internal/chatv2/...`. Fix any failures.

6. **Run full test suite**: Execute `go test ./... 2>&1 | grep -E "(ok|FAIL|---)"` — confirm no new failures introduced. Pre-existing failures in `internal/web` and `internal/identity` are known and should be ignored.

7. **Build**: Run `go build ./cmd/llmapiproxy` to verify compilation.

8. **Manual verification**: Start the server (`./llmapiproxy serve`) and test the new endpoints with `curl`. Verify request/response format, status codes, and error handling.

9. **Commit**: Commit with a descriptive conventional commit message.

## Key Conventions

- **Error wrapping**: Use `fmt.Errorf("...: %w", err)` for error chains
- **Logging**: Use zerolog (`log "github.com/rs/zerolog/log"`) — never `fmt.Println`
- **Naming**: `NewXxx()` constructors return concrete pointer types
- **HTTP handlers**: Methods on struct types, registered via Chi router
- **SQLite**: Use `modernc.org/sqlite` (same as existing stats/chat stores)
- **Schema**: Create tables in `init()` with `CREATE TABLE IF NOT EXISTS`

## Example Handoff

```json
{
  "salientSummary": "Implemented chatv2 store with sessions, messages, and model_defaults tables. Added CRUD handlers for all /ui/chatv2/ routes. All 24 tests pass, go vet clean, build succeeds.",
  "whatWasImplemented": "internal/chatv2/store.go with sessions/messages/model_defaults schema and CRUD methods. internal/web/chatv2_handlers.go with handlers for GET/POST/PUT/DELETE routes. Route registration in cmd/llmapiproxy/serve.go under /ui/chatv2/.",
  "whatWasLeftUndone": "",
  "verification": {
    "commandsRun": [
      {"command": "go test ./internal/chatv2/...", "exitCode": 0, "observation": "24 tests pass"},
      {"command": "go vet ./internal/chatv2/...", "exitCode": 0, "observation": "clean"},
      {"command": "go build ./cmd/llmapiproxy", "exitCode": 0, "observation": "builds successfully"},
      {"command": "curl -sf http://localhost:8000/ui/chatv2/sessions", "exitCode": 0, "observation": "returns empty JSON array"}
    ],
    "interactiveChecks": []
  },
  "tests": {
    "added": [
      {"file": "internal/chatv2/store_test.go", "cases": [
        {"name": "TestCreateSession", "verifies": "session created with UUID, default model, and timestamps"},
        {"name": "TestCRUDSession", "verifies": "create, get, update, delete session lifecycle"},
        {"name": "TestSaveAndGetMessages", "verifies": "messages saved in order and retrieved correctly"},
        {"name": "TestSearchSessions", "verifies": "FTS search returns matching sessions by title and message content"},
        {"name": "TestModelDefaults", "verifies": "per-model defaults saved and retrieved"}
      ]}
    ]
  },
  "discoveredIssues": []
}
```

## When to Return to Orchestrator

- Feature depends on a type/interface that doesn't exist yet and can't be created within this feature's scope
- Route registration requires changes to the main server setup that another worker is modifying
- Database migration issues that require architectural decisions
