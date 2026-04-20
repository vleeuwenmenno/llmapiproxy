---
name: frontend-worker
description: Builds Go templates, Alpine.js components, CSS — the chat UI layer
---

# Frontend Worker

NOTE: Startup and cleanup are handled by `worker-base`. This skill defines the WORK PROCEDURE.

## When to Use This Skill

Features that involve:
- Creating or modifying Go HTML templates (`.html` files in `internal/web/templates/`)
- Writing Alpine.js component logic (JavaScript)
- Creating CSS stylesheets
- Modifying the navbar template
- Frontend behavior: streaming, popovers, sidebar, keyboard shortcuts, markdown rendering

## Required Skills

- `agent-browser` — for manual browser verification of UI interactions after implementation

## Work Procedure

1. **Read context files first**: Read `mission.md`, `AGENTS.md`, `.factory/library/architecture.md` for architectural guidance. Read `.factory/research/alpinejs.md` if it exists for Alpine.js + Go template patterns.

2. **Understand existing patterns**: Read the existing chat template (`internal/web/templates/chat.html`) and CSS (`internal/web/static/chat.css`) for style conventions. Read `internal/web/static/shared.js` for utility functions. Read `internal/web/templates/navbar.html` for the nav structure.

3. **Implement template with `[[ ]]` delimiters**: The chatv2 template uses `[[ ]]` delimiters instead of `{{ }}` to avoid conflicts with Alpine.js. This template is loaded as a **separate template set** in `internal/web/web.go` — do NOT mix with existing `{{ }}` templates.

4. **Alpine.js component pattern**:
   - Define component logic in a separate JS file (`internal/web/static/chatv2.js`)
   - Use `Alpine.data('chatApp', () => ({...}))` for the main chat component
   - Use `Alpine.store('chat')` for global state (sessions, model, sidebar)
   - Use `Alpine.$persist()` for user preferences (model, sidebar state)
   - Handle streaming with `fetch() + ReadableStream` (NOT EventSource — need POST + auth headers)
   - Implement AbortController for stream cancellation
   - Use `$nextTick()` for DOM-dependent operations after state changes
   - Force reactivity on array mutations: `this.messages = [...this.messages]`

5. **CSS conventions**:
   - Use CSS variables from `main.css` (`--accent`, `--bg`, `--surface`, etc.)
   - Support dark/light theme (use CSS variables, never hardcode colors)
   - Mobile-first responsive design
   - Safe area insets for mobile: `env(safe-area-inset-bottom)`
   - No Tailwind — custom CSS only

6. **Verify with agent-browser**: After implementation, start the server and use `agent-browser` to:
   - Load the chatv2 page
   - Test the specific UI feature (click buttons, type text, verify elements)
   - Take screenshots as evidence
   - Verify dark/light theme rendering
   - Check mobile responsiveness (narrow viewport)

7. **Run Go tests**: `go test ./internal/web/...` — confirm no new failures. Pre-existing failures are known.

8. **Build**: `go build ./cmd/llmapiproxy` — verify compilation.

9. **Commit**: Commit with a descriptive conventional commit message.

## Key Conventions

- **Template delimiters**: `[[ ]]` for chatv2 templates ONLY
- **Alpine.js CDN**: `<script defer src="https://cdn.jsdelivr.net/npm/alpinejs@3.x.x/dist/cdn.min.js"></script>` — load AFTER plugins
- **Plugins**: Load `@alpinejs/persist` and `@alpinejs/focus` BEFORE the core Alpine script
- **x-cloak**: Add `[x-cloak] { display: none !important; }` to CSS and `x-cloak` to root element
- **No `{{ }}`** in chatv2 templates — use `[[ ]]` exclusively
- **Markdown**: Use `marked.js` (existing CDN) for rendering. Add `highlight.js` for syntax highlighting. Add `KaTeX` for LaTeX.
- **Streaming**: Use `fetch()` with `ReadableStream` — NOT `EventSource` (needs POST + auth headers)

## Alpine.js + Go Template Pattern

```html
<!-- Server data injection -->
<script type="application/json" id="chat-init-data">
  [[ .InitialData | json ]]
</script>

<!-- Alpine component -->
<div x-data="chatApp()" x-init="init()" x-cloak>
  <!-- UI elements using Alpine directives -->
</div>

<script src="/ui/static/chatv2.js"></script>
```

```javascript
// chatv2.js
function chatApp() {
  return {
    messages: [],
    input: '',
    streaming: false,
    
    init() {
      const data = JSON.parse(document.getElementById('chat-init-data').textContent);
      this.sessions = data.sessions || [];
      this.selectedModel = this.$persist(data.defaultModel || '').as('chatv2-model');
    },
    
    async sendMessage() { ... },
    async streamResponse(messages) { ... },
    abortStream() { ... },
  }
}
```

## Example Handoff

```json
{
  "salientSummary": "Implemented chatv2 Alpine.js template with streaming, model selector popover, and sidebar. Template uses [[ ]] delimiters. All UI features verified via agent-browser.",
  "whatWasImplemented": "internal/web/templates/chatv2.html with Alpine.js directives, internal/web/static/chatv2.js with chatApp component (streaming, abort, sidebar, model picker), internal/web/static/chatv2.css with responsive styles.",
  "whatWasLeftUndone": "",
  "verification": {
    "commandsRun": [
      {"command": "go build ./cmd/llmapiproxy", "exitCode": 0, "observation": "builds successfully"}
    ],
    "interactiveChecks": [
      {"action": "Navigate to /ui/chatv2/, type message, press Enter", "observed": "Message sent, streaming response received, auto-scroll works"},
      {"action": "Click model chip, search for 'gpt', select model", "observed": "Popover opens, filters list, model selected and persisted"},
      {"action": "Press Ctrl+K", "observed": "Model picker opens with search focused"},
      {"action": "Click sidebar toggle, click session", "observed": "Sidebar opens, session loads with messages"},
      {"action": "During streaming, click Stop button", "observed": "Stream aborted, partial response preserved"},
      {"action": "Switch to light theme", "observed": "All elements render correctly in light mode"}
    ]
  },
  "tests": {
    "added": []
  },
  "discoveredIssues": []
}
```

## When to Return to Orchestrator

- Backend API endpoint the UI depends on doesn't exist yet (return to orchestrator to prioritize backend work)
- Template delimiter change affects other templates (needs coordination)
- CSS variable naming conflicts with existing styles
