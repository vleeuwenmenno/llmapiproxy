# User Testing

Testing surface, required testing skills/tools, and resource cost classification.

**What belongs here:** Testing surface info, required tools, concurrency limits, testing gotchas.
**What does NOT belong here:** Service ports/commands (use `.factory/services.yaml`).

---

## Validation Surface

- **Primary Surface**: Browser at `http://localhost:8000/ui/chatv2/`
- **Secondary Surface**: API endpoints under `/ui/chatv2/` (testable via `curl`)
- **Auth**: Login required — navigate to `/ui/login` first, use configured credentials
- **Tool**: `agent-browser` for browser-based validation, `curl` for API-only assertions

## Validation Concurrency

- **Max concurrent agent-browser validators**: 5
- **Rationale**: App is lightweight (~161 MiB RSS idle). Machine has 46 GB RAM, 24 cores. Each browser instance adds ~300 MB. 5 instances = ~1.5 GB, well within budget (70% of ~34 GB available headroom = ~23.8 GB).
- **Note**: Each validator needs its own browser session with separate auth cookie

## Testing Gotchas

- Server must be running on :8000 before validation
- Login is required for all UI pages (except `/health` and `/ui/login`)
- The existing chat page at `/ui/chat` must remain functional (not modified)
- Streaming assertions require a backend with a valid API key configured
- For empty-response and error assertions, the backend must be configured to produce those states
