# Identity Spoofing

Identity spoofing makes the proxy's outgoing requests look like they came from a specific CLI tool. This sets the `User-Agent` header and adds custom headers on upstream requests.

> **⚠️ Warning:** Identity spoofing is best-effort. It mimics CLI tool request signatures but cannot guarantee compatibility. Providers may still detect proxy usage and restrict your access.

## Built-in Profiles

| Profile          | Simulates                         | User-Agent              |
| ---------------- | --------------------------------- | ----------------------- |
| `none`           | Passthrough (no spoofing)         | Default Go HTTP client  |
| `codex-cli`      | OpenAI Codex CLI v0.120.0         | `codex-cli/0.120.0`     |
| `gemini-cli`     | Google Gemini CLI v0.38.0         | `gemini-cli/0.38.0`     |
| `copilot-vscode` | GitHub Copilot VS Code extension  | VS Code Copilot headers |
| `opencode`       | OpenCode CLI v1.4.6               | `opencode/1.4.6`        |
| `claude-code`    | Anthropic Claude Code CLI v1.0.22 | Claude Code headers     |

## Configuration

### Global Profile

Applies to all backends unless overridden:

```yaml
identity_profile: "opencode"
```

### Per-Backend Override

Override the global profile for specific backends:

```yaml
identity_profile: "opencode"

backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "..."
    identity_profile: "codex-cli" # Override for this backend

  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "..."
    # Uses global profile: "opencode"
```

### Custom Profiles

Define your own profiles with templated User-Agent strings and headers:

```yaml
identity_profile: "my-tool"

custom_identity_profiles:
  - id: "my-tool"
    display_name: "My Custom Tool"
    user_agent: "my-tool/1.0 ({{.OS}}; {{.Arch}})"
    headers:
      X-Custom-Header: "my-value"
      X-Client-Version: "1.0.0"
```

**Available template variables:**

| Variable         | Description            | Example           |
| ---------------- | ---------------------- | ----------------- |
| `{{.OS}}`        | Operating system       | `linux`, `darwin` |
| `{{.Arch}}`      | CPU architecture       | `amd64`, `arm64`  |
| `{{.Model}}`     | Model being requested  | `gpt-4o`          |
| `{{.SessionID}}` | Per-process session ID | `abc123...`       |
| `{{.Platform}}`  | OS/Arch combined       | `linux/amd64`     |

The session ID is generated once at startup and reused across all requests for the lifetime of the process.

## Web UI

Identity profiles can also be managed from the web UI:

| Endpoint                               | Method | Description                 |
| -------------------------------------- | ------ | --------------------------- |
| `/ui/json/identity-profiles`           | GET    | List all available profiles |
| `/ui/identity-profile`                 | POST   | Set the global profile      |
| `/ui/backends/{name}/identity-profile` | POST   | Set a per-backend profile   |

## How It Works

When identity spoofing is enabled:

1. The proxy intercepts outgoing HTTP requests to backends
2. The `User-Agent` header is replaced with the profile's value
3. Any additional headers from the profile are added
4. These headers are sent **in addition to** any `extra_headers` configured on the backend

The spoofing applies to all upstream requests: chat completions, model listing, and streaming responses.
