# OAuth Web UI

## Overview

The OAuth management UI is integrated into the settings page (`/ui/settings`) as an "OAuth Connections" section. It displays real-time authentication status for all OAuth backends (Copilot, Codex).

## Key Files

- `internal/web/templates/settings.html` - Settings page with OAuth section
- `internal/web/templates/oauth_status.html` - HTMX fragment for live status
- `internal/web/web.go` - OAuth handler methods (OAuthStatus, OAuthLogin, OAuthCallback, OAuthDisconnect)
- `internal/backend/backend.go` - OAuthStatus struct with TokenState field
- `internal/backend/copilot.go` - CopilotBackend.OAuthStatus() implementation
- `internal/backend/codex.go` - CodexBackend.OAuthStatus() implementation

## Endpoints

- `GET /ui/oauth/status` - HTMX fragment with live auth status for all OAuth backends
- `GET /ui/oauth/login/{backend}` - Initiates OAuth flow (Codex: PKCE redirect)
- `GET /ui/oauth/callback/{backend}` - Handles OAuth callback
- `POST /ui/oauth/disconnect/{backend}` - Clears stored tokens

## OAuthStatus Fields

- `TokenState`: Visual indicator state ("valid", "expiring", "expired", "missing")
  - "valid": Token is valid and not near expiry (green dot)
  - "expiring": Token expires within 5 minutes (yellow dot)
  - "expired": Token has expired (red dot)
  - "missing": No token stored at all (red dot)
- `TokenSource`: Where the token came from (e.g., "env:GH_TOKEN", "codex_oauth", "gh_cli")
- `TokenExpiry`: RFC3339 formatted expiry time
- `LastRefresh`: RFC3339 formatted last refresh time
- `NeedsReauth`: True only when token exists but expired and can't be refreshed

## Template Architecture

The OAuth section uses HTMX auto-refresh:
- `hx-get="/ui/oauth/status"` polls every 30 seconds
- `hx-swap="innerHTML"` replaces the container content
- The outer `<div id="oauth-status-container">` wraps all backend cards

## Testing Notes

- Token files are stored relative to CWD in `tokens/` directory
- Tests must clear pre-existing tokens with `Disconnect()` before testing "not connected" state
- HTML template encodes `+` in timestamps as `&#43;` - tests must account for this
- 16 tests in `internal/web/web_test.go` cover all handler behaviors
