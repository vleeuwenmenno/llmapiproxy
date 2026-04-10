# LLM API Proxy Feature Expansion Plan

This document captures the next phase of work for llmapiproxy so it can be resumed on another machine without needing the conversation context.

## Project Goal

Extend the current OpenAI-compatible proxy and embedded UI with:

1. Better error visibility for failed upstream requests.
2. Client identification and per-client stats based on proxy API keys.
3. Optional per-client backend API key overrides.
4. Model overlap routing controls and fallback strategies.
5. On-demand quota and balance display for supported backends.
6. UI polish additions such as Kilocode support and a better curl/HTTP icon.

## Core Design Decisions

- Clients are identified by the proxy API key they use, not by a custom request header.
- Every proxy API key belongs to a named client.
- Backend API key overrides are optional.
- If a client does not define an override for a backend, the backend's default API key from the backend configuration is used.
- Quota lookups are on-demand only and are triggered by user action in the UI.
- Error response bodies should capture only the first 4 KB of upstream output.
- SQLite remains the persistence layer for stats and new schema fields.

## Suggested Execution Order

1. Sprint 1: error detail capture.
2. Sprint 2: client identification and client-scoped keys.
3. Sprint 3: overlap routing and fallback policy.
4. Sprint 4: quota and balance display.

Sprint 1 and Sprint 4 can be developed independently. Sprint 2 depends on the auth/config changes established in this plan. Sprint 3 depends on the routing and model metadata foundations already in the codebase.

---

## Sprint 1 - Error Detail Capture + Clickable Error Rows

### Goal

When an upstream request fails, store the upstream error payload in SQLite and expose it in a modal from the dashboard so the failure can be inspected without leaving the UI.

### Why this matters

The current dashboard shows that an error happened, but not why. For OpenAI-compatible APIs, the most useful information is usually in the upstream JSON error payload. Keeping a short excerpt of that payload makes debugging dramatically easier, especially for authentication issues, invalid model names, rate limits, and provider-specific failures.

### Implementation steps

1. Extend the stats record model with an error response body field.
   - Add a `ResponseBody string` field to `stats.Record`.
   - Keep it optional in the JSON representation so old callers do not break.
   - Preserve the existing fields and stats calculations.

2. Extend the SQLite schema.
   - Add a `response_body` column to the `requests` table.
   - Use a migration path that works for existing installs.
   - Prefer a `PRAGMA user_version` based migration so the database can evolve safely.
   - Ensure the new column defaults to an empty string for older rows.

3. Capture the upstream response body in the proxy handler.
   - In the non-streaming failure path, read up to 4 KB from the upstream response body before returning the proxy error.
   - In the streaming failure path, capture enough context to show the upstream error when the stream cannot begin or closes with a failure.
   - Preserve the existing error handling behavior while adding the new capture logic.

4. Add a request detail endpoint to the UI.
   - Create a handler that accepts request identifiers.
   - Return a fragment suitable for a modal.
   - Render the timestamp, backend, model, status code, latency, token counts, and captured error body.

5. Add a request detail template.
   - Show the payload in a readable, scrollable panel.
   - If the body looks like JSON, present it in a code block style that is easy to scan.
   - If the body is empty, show a clear empty-state message.

6. Make error rows clickable in the stats fragment.
   - Only failed rows should open the detail modal.
   - The row should issue an htmx request to the detail endpoint.
   - Add a modal container to the dashboard template if one is not already present.

7. Register the new route.
   - Add `/ui/stats/detail` under the UI route group.

### Acceptance criteria

- A failed upstream request creates a stats record containing a truncated response body.
- Clicking a failed row opens a detail modal.
- Existing stats and dashboard behavior continue to work.
- Older databases migrate without manual intervention.

### Verification

- Trigger a failure with an invalid or unavailable model.
- Confirm the dashboard shows the failed request.
- Open the row and confirm the modal shows the upstream payload.

---

## Sprint 2 - Client Identification via API Keys + Per-Client Stats + Backend Key Overrides

### Goal

Replace flat proxy API key management with named clients. Identify clients by the proxy API key they use, track stats per client, and allow optional backend-specific API key overrides for each client.

### Why this matters

The earlier `X-Client-Name` idea is not practical because most OpenAI-compatible clients do not support arbitrary custom headers. API keys are universally supported, which makes them the natural identity mechanism. This also gives each client a stable identity that can be used in stats and configuration.

### Configuration model

The client model becomes the source of truth.

Example:

```yaml
clients:
  - name: "zed"
    api_key: "sk-proxy-zed-123"
  - name: "continue"
    api_key: "sk-proxy-cont-456"
    backend_keys:
      openrouter: "sk-or-client-specific"
```

Behavior:

- A client must have a `name`.
- A client must have a proxy `api_key` used for authentication.
- `backend_keys` is optional.
- If `backend_keys` is omitted or if a specific backend is not listed, the backend uses its default `api_key` from the existing backend configuration.

### Implementation steps

1. Replace the flat server API key list with client records.
   - Remove or deprecate `server.api_keys`.
   - Add `clients` to the top-level config model.
   - Define `ClientConfig` with at least `Name`, `APIKey`, and `BackendKeys`.
   - Update config validation to require unique client names and non-empty keys.
   - Add a helper for looking up a client by token.

2. Update the authentication middleware.
   - Change middleware input from `[]string` keys to the list of client configs.
   - Match the incoming Bearer token to a configured client.
   - Reject unknown keys with the same auth error shape used today.
   - Store the resolved client name in request context.
   - Provide a small helper to read the client from context later in the handler.

3. Add client information to stats records.
   - Extend `stats.Record` with a `Client` field.
   - Persist that field in SQLite.
   - Add a database migration for the new column.
   - Default the client label to something safe like `unknown` when no match exists.

4. Record the client name in the proxy handler.
   - Read the client from context as early as possible.
   - Populate every stats record with the resolved client name.
   - Keep handler behavior identical otherwise.

5. Support optional backend key overrides per client.
   - Resolve the target backend as today.
   - Before dispatching the request, check whether the client has an override for that backend.
   - If an override exists, use it for the upstream request only.
   - If not, use the backend's normal configured key.
   - Keep the override behavior purely additive so existing setups continue to work.

6. Extend the stats summary with client breakdowns.
   - Add request counts by client.
   - Add token totals by client.
   - Keep the existing backend and model summary behavior unchanged.

7. Update the dashboard with a client section.
   - Show client request volume.
   - Show client token usage.
   - Make the section compact enough to stay useful alongside backend and model summaries.

8. Replace the current settings page key management with client management.
   - Show a list of configured clients instead of a single flat key list.
   - Display the client name, a masked view of the proxy key, and any backend overrides.
   - Add forms to create, edit, and delete clients.
   - Keep the raw YAML config editor as a power-user fallback.
   - Update the save path so edits persist back to the config file and reload cleanly.

9. Wire the new client model into startup.
   - Pass the client list into auth middleware when routes are built.
   - Ensure config reload behavior still updates the in-memory client list.

10. Add Kilocode support to the models page.
    - Move the Kilocode icon assets into the embedded static icons folder.
    - Add a Kilocode quick-connect button and config example.
    - Make sure it matches the existing Quick Connect modal pattern.

11. Replace the curl/HTTP icon.
    - Use a more generic terminal-style icon instead of the current logo-based fallback.
    - Keep the button label and behavior unchanged.

### Acceptance criteria

- Requests authenticate by proxy API key.
- The resolved client name appears in stats and summaries.
- Clients can define optional backend overrides.
- If no override is present, the backend default API key is used.
- The settings page can manage clients without editing YAML manually, while still allowing raw config editing.

### Verification

- Create two clients with different API keys.
- Send requests with each key and confirm the dashboard shows distinct client stats.
- Configure a backend override for one client and confirm the proxy forwards the override key.

---

## Sprint 3 - Model Overlap Fallback + Priority Management

### Goal

When a model exists in multiple backends, allow explicit configuration of priority order and fallback behavior, and expose that configuration through the models UI.

### Why this matters

Right now, overlapping models are visible, but routing behavior is still mostly implicit. Once multiple backends expose the same model ID, users need a clear way to choose which backend should be tried first and what should happen if that backend is unavailable.

### Implementation steps

1. Add routing configuration to the config model.
   - Introduce a top-level `routing` section.
   - Store a default overlap strategy and fallback timeout.
   - Allow per-model overrides keyed by bare model ID.
   - Support strategy values such as priority, round-robin, and lowest-latency.

2. Extend model resolution.
   - Update backend resolution so overlapping models can return a route definition instead of a single backend.
   - Preserve the current single-backend behavior when there is no overlap.
   - Use the routing config as the source of truth for overlap decisions.

3. Implement fallback execution in the proxy handler.
   - If the preferred backend fails or times out, try the next backend in the configured order.
   - Track which backend eventually succeeded in the stats record.
   - Keep the external API behavior stable.

4. Redesign the overlap cards in the models page.
   - Show the list of backends that provide each overlapping model.
   - Make the card visually clear enough that the overlap is obvious at a glance.
   - Make the card open a configuration modal.

5. Build the overlap detail modal.
   - Show backend priority order.
   - Allow the strategy to be changed.
   - Allow the fallback timeout to be edited.
   - Keep the UI simple enough to edit without understanding internal routing details.

6. Add a save endpoint for routing config.
   - Allow saving only the routing portion of the config.
   - Validate the update before writing it back.
   - Reload configuration after save.

### Acceptance criteria

- Overlapping models can be assigned a priority order.
- A backend failure can fall back to the next configured backend.
- The model card UI clearly communicates overlaps and routing order.
- Routing edits persist to config and take effect after reload.

### Verification

- Configure a model that exists in two backends.
- Force the first backend to fail.
- Confirm the request falls back to the next backend.
- Change the order in the UI and confirm the new order is honored.

---

## Sprint 4 - Backend Quota / Balance Display

### Goal

Add an on-demand quota and balance panel for backends that expose usage APIs, starting with OpenRouter and Z.ai.

### Why this matters

Users need a quick way to understand remaining quota and reset timing without opening another dashboard or polling continuously. On-demand refresh keeps the proxy lightweight while still surfacing useful operational data.

### Implementation steps

1. Define a quota abstraction.
   - Create a small quota provider interface in a new package.
   - Include fields for limit, remaining, usage, reset time, and raw payload.
   - Keep the abstraction minimal so adding more providers later is straightforward.

2. Implement OpenRouter quota support.
   - Call the OpenRouter key endpoint with Bearer auth.
   - Parse the usage and remaining-limit values needed by the UI.
   - Preserve the full response for debugging when practical.

3. Implement Z.ai quota support.
   - Call the quota endpoint with Bearer auth.
   - Parse the limit entries and reset information.
   - Handle provider-specific response variations defensively.

4. Add config support for quota lookups.
   - Add an optional field to backend config if needed for explicit quota URLs.
   - Provide auto-detection based on provider or base URL where possible.
   - Keep quota support opt-in by UI action rather than background polling.

5. Add a quota fragment endpoint.
   - Fetch quota information for enabled backends in parallel.
   - Apply a sane timeout per backend.
   - Return a fragment suitable for htmx replacement.

6. Add the dashboard quota section.
   - Show quota status near the top of the dashboard.
   - Add a refresh button that fetches on demand.
   - Use color and text to make healthy, warning, and critical states obvious.

### Acceptance criteria

- Clicking refresh fetches quota information on demand.
- OpenRouter and Z.ai quotas display correctly when configured.
- The UI handles unsupported or failing quota lookups gracefully.
- No background polling is introduced.

### Verification

- Configure valid OpenRouter and Z.ai credentials.
- Open the dashboard and refresh the quota section.
- Confirm the remaining balance and reset timing display correctly.

---

## File Impact Summary

Likely files touched by these sprints:

- `internal/stats/stats.go`
- `internal/stats/store.go`
- `internal/proxy/handler.go`
- `internal/proxy/middleware.go`
- `internal/config/config.go`
- `internal/backend/backend.go`
- `internal/backend/openai.go`
- `internal/backend/registry.go`
- `internal/web/web.go`
- `internal/web/templates/dashboard.html`
- `internal/web/templates/models.html`
- `internal/web/templates/settings.html`
- `internal/web/templates/stats_fragment.html`
- `cmd/llmapiproxy/main.go`
- `internal/quota/` new package

## Known Risks

- SQLite migration code must be careful to preserve existing data.
- Per-client backend override behavior should not accidentally break default backend auth.
- The quota APIs may vary or change, especially the Z.ai endpoint.
- Overlap fallback logic needs clear timeout and retry boundaries to avoid hanging requests.
- The settings UI will become more complex once clients replace the flat key list, so edits should be kept incremental.

## Recommended Milestones

1. Complete Sprint 1 and verify failure inspection end to end.
2. Complete Sprint 2 and migrate the config model to client-owned API keys.
3. Complete Sprint 3 and test fallback routing with overlapping models.
4. Complete Sprint 4 and verify live quota refresh.

## Notes For Resuming Later

- The central config change is the move from a flat `server.api_keys` list to named `clients`.
- Backend key overrides are optional, not required.
- The client name should come from the resolved proxy key, not from a request header.
- If there is any uncertainty while implementing, keep the config and UI aligned first, then thread the data through middleware and handlers.
