# Environment

Environment variables, external dependencies, and setup notes.

**What belongs here:** Required env vars, external API keys/services, dependency quirks, platform-specific notes.
**What does NOT belong here:** Service ports/commands (use .factory/services.yaml).

---

## Environment Variables

### GitHub Copilot Device Code Flow
- Client ID: Iv1.b507a08c87ecfe98 (VS Code Copilot extension)
- Device code endpoint: POST https://github.com/login/device/code
- Token endpoint: POST https://github.com/login/oauth/access_token
- Scope: read:user
- Copilot token exchange: GET https://api.github.com/copilot_internal/v2/token
- Requires active GitHub Copilot subscription
- Token is long-lived, validated on-demand (no auto-refresh)

### OpenAI Codex OAuth
- Uses public client ID `app_EMoamEEZ73f0CkXaXp7hrann`
- Callback URL: `http://localhost:8000/ui/oauth/callback/codex`
- Scopes: `openid profile email offline_access`
- Auth endpoint: `https://auth.openai.com/oauth/authorize`
- Token endpoint: `https://auth.openai.com/oauth/token`

### OpenAI Codex Device Code Flow (Alternative)
- Same client_id as PKCE flow: app_EMoamEEZ73f0CkXaXp7hrann
- Device code endpoint: POST https://auth.openai.com/oauth/device/code
- Token endpoint: POST https://auth.openai.com/oauth/token
- For headless/SSH environments where browser callback won't work

## External Dependencies

- **GitHub API** (`api.github.com`): Copilot token exchange. Requires active Copilot subscription.
- **Copilot API** (`api.githubcopilot.com`): Chat completions, model listing.
- **OpenAI Auth** (`auth.openai.com`): Codex OAuth PKCE flow.
- **Codex API** (`chatgpt.com`): Responses API for chat completions.
- **`gh` CLI**: Optional, used for GitHub token discovery. User `Aeversil` is authenticated.

## Platform Notes

- macOS (darwin/arm64), Go 1.25.8
- `gh` CLI installed and authenticated (user: Aeversil, scopes: gist, read:org, repo)
- Port 8000 available for proxy
- `config.yaml` is gitignored — never commit secrets
