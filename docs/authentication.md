# Authentication & Users

LLM API Proxy has two separate authentication systems:

1. **API Key Auth** — For the proxy API (`/v1/*` endpoints). Used by clients like VS Code, Cursor, scripts.
2. **Web UI Auth** — For the dashboard (`/ui/*` endpoints). Optional username/password login.

---

## API Key Authentication

All requests to `/v1/*` endpoints require a valid API key. Keys are configured in `server.api_keys`:

```yaml
server:
  api_keys:
    - "my-secret-proxy-key"
    - "another-key-for-scripts"
```

Clients must include the key in one of two headers:

```
Authorization: Bearer my-secret-proxy-key
```

or

```
x-api-key: my-secret-proxy-key
```

### Named Clients

For better analytics tracking, configure named clients:

```yaml
clients:
  - name: "vscode"
    api_key: "key-for-vscode"

  - name: "scripts"
    api_key: "key-for-scripts"

  - name: "work"
    api_key: "key-for-work"
    backend_keys: # Override backend API keys per client
      openrouter: "sk-or-v1-work-account"
```

The client name appears in analytics and request logs, making it easy to see which tool made which request.

### Per-Client Backend Key Overrides

The `backend_keys` option lets different clients use different provider API keys. For example, if you have personal and work OpenRouter accounts:

```yaml
clients:
  - name: "personal"
    api_key: "personal-proxy-key"
    backend_keys:
      openrouter: "sk-or-v1-personal-key"

  - name: "work"
    api_key: "work-proxy-key"
    backend_keys:
      openrouter: "sk-or-v1-work-key"
```

### Playground Client

The built-in Playground page uses a special client named `playground`. Add it to your clients list to enable playground access:

```yaml
clients:
  - name: playground
    api_key: "sk-proxy-internal-playground-<random>"
```

The playground client is automatically hidden from the Settings → API Keys page.

---

## Web UI Authentication

### Username/Password Auth (Full)

For multi-user deployments, enable web UI authentication:

```yaml
server:
  web_auth: true
  users_db_path: "data/users.db"
  web_auth_secret: "a-long-random-secret-here" # Persist sessions across restarts
```

| Option            | Type   | Default         | Description                                                                                                                                    |
| ----------------- | ------ | --------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `web_auth`        | bool   | `false`         | Enable web UI login                                                                                                                            |
| `users_db_path`   | string | `data/users.db` | SQLite database for users                                                                                                                      |
| `web_auth_secret` | string | random          | HMAC key for signing session cookies. If empty, a random key is generated on startup (sessions lost on restart). Set this to persist sessions. |

### User Management CLI

Manage users via the `user` command:

```bash
# Add a user (prompts for password interactively)
llmapiproxy user add alice

# Add with password via flag
llmapiproxy user add bob --password secret123

# List all users
llmapiproxy user list

# Change password
llmapiproxy user passwd alice

# Remove a user
llmapiproxy user remove bob

# Specify custom users database path
llmapiproxy --users-db /path/to/users.db user add alice
```

### Session System

- **Cookie name:** `llmproxy_session`
- **Duration:** 24 hours
- **Format:** Base64-encoded JSON (username, expiry, nonce) + HMAC-SHA256 signature
- **API routes** (`/v1/*`) are **never** affected by web auth — they always use API key auth

### First-Time Setup

When `web_auth` is enabled and no users exist:

1. Navigate to the web UI
2. You'll be redirected to `/ui/setup`
3. Create the first admin user
4. You'll be automatically logged in

### Login Flow

1. Navigate to `/ui/login`
2. Enter username and password
3. On success, a session cookie is set and you're redirected to the dashboard
4. Use `/ui/logout` to end the session

---

## Security Best Practices

- **Never commit** `config.yaml` with real API keys — use `config.example.yaml` as a template
- **Set `web_auth_secret`** if you use web auth, to prevent session invalidation on restart
- **OAuth tokens** are stored locally in `data/tokens/` — ensure this directory is not publicly accessible
- **Use HTTPS** in production (via a reverse proxy like Nginx or Caddy)
