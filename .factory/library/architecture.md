# ChatV2 (Chat Beta) Architecture

## 1. System Overview

ChatV2 is a new chat page at `/ui/chatv2/` that lives alongside the existing `/ui/chat/` page. It replaces the vanilla JS + HTMX approach with **Alpine.js** for reactivity, uses **separate Go template files with `[[ ]]` delimiters** (to avoid conflicts with Alpine's `{{ }}` syntax), defines **new API routes** under `/ui/chatv2/`, and stores data in its **own SQLite database** (`data/chatv2.db`).

The existing chat page and its database are **never modified** by chatv2 code.

## 2. Components

### 2.1 ChatV2 Store — `internal/chatv2/store.go`

New SQLite store backed by `data/chatv2.db`. Follows the same pattern as `internal/chat/store.go` but with an extended schema (adds `model_defaults` table, `tps` column on messages, etc.). Exposes methods for CRUD on sessions, messages, and model defaults.

### 2.2 ChatV2 Handlers — `internal/chatv2/handlers.go` (new file)

HTTP handler methods on a `Handler` struct (following the project's constructor pattern: `NewHandler()`). All handlers are registered in `cmd/llmapiproxy/serve.go` under the `/ui/chatv2/` route group. The handler holds a reference to the chatv2 store.

### 2.3 ChatV2 Template — `internal/web/templates/chatv2.html`

Full-page Go template using `[[ ]]` delimiters (configured via a separate `template.Template` instance, not mixed with the existing `{{ }}` template set). Contains Alpine.js directives (`x-data`, `x-show`, `x-on`, `x-model`, etc.) for all interactivity.

### 2.4 ChatV2 Static Assets

| File | Purpose |
|---|---|
| `internal/web/static/chatv2.css` | Scoped styles using existing CSS variables from `main.css` |
| `internal/web/static/chatv2.js` | Alpine.js component definition (the `chatApp()` function returning the Alpine data object) |

### 2.5 Navbar Modification — `internal/web/templates/navbar.html`

Add a "Chat (Beta)" link alongside the existing "Chat" link. The new link points to `/ui/chatv2/` and includes a small badge (e.g., `<span class="beta-badge">Beta</span>`).

## 3. Data Flows

### 3.1 Sending a Message

```
User types message in input
  → Alpine.js checks if session exists
    → No session: POST /ui/chatv2/sessions → create session → get session ID
  → POST /ui/chatv2/sessions/{id}/messages {role: "user", content: "..."} → save user message
  → POST /v1/chat/completions (existing proxy endpoint) with model, messages, stream: true
    → SSE stream back
    → Alpine.js renders tokens incrementally
    → On stream end: POST /ui/chatv2/sessions/{id}/messages {role: "assistant", content: full, tokens, prompt_tokens, duration_ms, tps}
  → Alpine.js updates session in sidebar
```

All LLM requests go through the **existing** `/v1/chat/completions` proxy endpoint. ChatV2 never talks to backends directly.

### 3.2 Model Selection

```
User clicks model selector
  → Alpine.js popover opens
  → GET /ui/chatv2/models → returns available models from registry
  → User selects model
  → Alpine state updated
  → PUT /ui/chatv2/sessions/{id} → persist model choice to session
```

### 3.3 Session Management

```
Page load → GET /ui/chatv2/sessions → populate sidebar
Click session → GET /ui/chatv2/sessions/{id} → load messages into Alpine state
New chat → POST /ui/chatv2/sessions → create, redirect Alpine state
Delete session → DELETE /ui/chatv2/sessions/{id} → remove, update sidebar
```

## 4. Database Schema

Database file: `data/chatv2.db`

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,           -- UUID
    title         TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL DEFAULT '',
    system_prompt TEXT    NOT NULL DEFAULT '',
    temperature   REAL    NOT NULL DEFAULT 0.7,
    top_p         REAL    NOT NULL DEFAULT 1.0,
    max_tokens    INTEGER NOT NULL DEFAULT 4096,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chatv2_sessions_updated ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role          TEXT    NOT NULL,              -- "user" | "assistant" | "system"
    content       TEXT    NOT NULL DEFAULT '',
    tokens        INTEGER NOT NULL DEFAULT 0,    -- completion tokens
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    duration_ms   REAL    NOT NULL DEFAULT 0,
    tps           REAL    NOT NULL DEFAULT 0,    -- tokens per second
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chatv2_messages_session ON messages(session_id);

CREATE TABLE IF NOT EXISTS model_defaults (
    model_id      TEXT PRIMARY KEY,
    temperature   REAL    NOT NULL DEFAULT 0.7,
    top_p         REAL    NOT NULL DEFAULT 1.0,
    max_tokens    INTEGER NOT NULL DEFAULT 4096,
    system_prompt TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);
```

**Differences from existing `chat.db` schema**: adds `model_defaults` table, adds `tps` column to `messages`, uses separate DB file.

## 5. API Routes

All routes are under `/ui/chatv2/` and require the same auth as existing UI routes.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/ui/chatv2/` | Serve chatv2.html page |
| GET | `/ui/chatv2/sessions` | List all sessions (JSON array of session summaries) |
| POST | `/ui/chatv2/sessions` | Create a new session (returns session JSON) |
| GET | `/ui/chatv2/sessions/{id}` | Get session + its messages (JSON) |
| PUT | `/ui/chatv2/sessions/{id}` | Update session fields (model, title, system_prompt, temperature, top_p, max_tokens) |
| DELETE | `/ui/chatv2/sessions/{id}` | Delete a session and its messages |
| DELETE | `/ui/chatv2/sessions` | Delete all sessions |
| GET | `/ui/chatv2/sessions/{id}/messages` | List messages for a session (JSON) |
| POST | `/ui/chatv2/sessions/{id}/messages` | Save a message to a session |
| POST | `/ui/chatv2/sessions/{id}/title` | Auto-generate session title using LLM |
| GET | `/ui/chatv2/models` | List available models from the registry (JSON) |
| GET | `/ui/chatv2/model-defaults` | Get all model defaults (JSON) |
| GET | `/ui/chatv2/model-defaults/{modelId}` | Get defaults for a specific model |
| PUT | `/ui/chatv2/model-defaults/{modelId}` | Set defaults for a model |

Route registration happens in `cmd/llmapiproxy/serve.go`, inside the existing `r.Route("/ui", ...)` block, as a new sub-group:

```go
r.Route("/chatv2", func(r chi.Router) {
    // chatv2 routes here
})
```

## 6. Key Invariants

1. **Existing chat is untouched** — The page at `/ui/chat/`, the `internal/chat/` package, and `data/chat.db` are never modified by chatv2 code.
2. **Proxy reuse** — Chatv2 sends LLM requests through the existing `/v1/chat/completions` proxy endpoint, never directly to backends.
3. **No build step** — Alpine.js is loaded from CDN. All JS is vanilla files served statically.
4. **Template isolation** — Chatv2 templates use `[[ ]]` delimiters via a separate `template.Template` instance. They are not mixed into the existing template set that uses `{{ }}`.
5. **Auth consistency** — Chatv2 routes use the same auth middleware as existing UI routes (server-side API key).
6. **CSS variable reuse** — Chatv2 CSS uses the same CSS custom properties defined in `main.css` for colors, spacing, and theming.

## 7. Technology Details

| Component | Technology | Notes |
|-----------|-----------|-------|
| Server | Go 1.25+ with Chi router | Same as existing |
| Database | SQLite via `modernc.org/sqlite` | Same driver as existing; separate DB file |
| Reactivity | Alpine.js 3.x via CDN | `<script defer src="https://cdn.jsdelivr.net/npm/alpinejs@3.x.x/dist/cdn.min.js"></script>` |
| Alpine Plugins | Persist, Focus | `@alpinejs/persist` for remembering sidebar state; `@alpinejs/focus` for accessibility |
| Markdown | marked.js via CDN | Already used in existing chat |
| Syntax Highlighting | highlight.js via CDN | New addition for code blocks |
| LaTeX | KaTeX via CDN + auto-render | New addition for math expressions |
| CSS | Custom CSS with existing variables | No preprocessor; uses `var(--color-*)` etc. from `main.css` |
| UUIDs | `github.com/google/uuid` | Same as existing chat store |
| Logging | `github.com/rs/zerolog` | Same as entire project |
