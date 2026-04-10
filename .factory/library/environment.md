# Environment

Environment variables, external dependencies, and setup notes.

**What belongs here:** Required env vars, external API keys/services, dependency quirks, platform-specific notes.
**What does NOT belong here:** Service ports/commands (use .factory/services.yaml).

---

## Environment Variables

### GitHub Token Discovery (Copilot)
The proxy discovers GitHub tokens in this priority order:
1. `COPILOT_GITHUB_TOKEN` — Copilot-specific override
2. `GH_TOKEN` — Standard GitHub CLI variable
3. `GITHUB_TOKEN` — CI/CD standard
4. `gh auth token` command — Requires `gh` CLI installed and authenticated
5. `~/.config/gh/hosts.yml` — Persistent gh credentials
6. Persisted token file — From previous OAuth flow

### OpenAI Codex OAuth
- Uses public client ID `app_EMoamEEZ73f0CkXaXp7hrann`
- Callback URL: `http://localhost:8000/ui/oauth/callback/codex`
- Scopes: `openid profile email offline_access`
- Auth endpoint: `https://auth.openai.com/oauth/authorize`
- Token endpoint: `https://auth.openai.com/oauth/token`

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
