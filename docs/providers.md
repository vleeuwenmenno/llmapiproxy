# Provider Setup Guides

LLM API Proxy works with any provider implementing the OpenAI API format.

## OpenRouter

[openrouter.ai](https://openrouter.ai) — Access 200+ models from various providers.

```yaml
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."
    extra_headers:
      HTTP-Referer: "https://yourdomain.com"
      X-Title: "Your App Name"
```

Models can be referenced with their full path:

```
openrouter/anthropic/claude-sonnet-4
openrouter/openai/gpt-4o
```

## Z.ai

[z.ai](https://z.ai) — GLM models from Zhipu AI.

```yaml
backends:
  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "..."
    models:
      - glm-5.1
      - glm-5-turbo
      - glm-5
      - glm-4.7
      - glm-4.6v
      - glm-4.5-air
```

## OpenCode (Zen & Go)

[opencode.ai](https://opencode.ai) — Curated coding models.

**Zen** (pay-as-you-go):

```yaml
backends:
  - name: zen
    type: openai
    base_url: https://api.opencode.ai/zen/v1
    api_key: "..."
```

**Go** (subscription):

```yaml
backends:
  - name: go
    type: openai
    base_url: https://api.opencode.ai/go/v1
    api_key: "..."
```

## OpenAI

```yaml
backends:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key: "sk-..."
    models:
      - gpt-4o
      - gpt-4o-mini
      - gpt-4-turbo
```

## OpenAI Codex

[chatgpt.com](https://chatgpt.com) — OpenAI's Codex backend for agentic coding models.

The proxy authenticates via OAuth PKCE (browser) or Device Code Flow (headless/SSH).
Models are **dynamically fetched** from the Codex API when a valid token is available,
falling back to a built-in default list when offline or unauthenticated.

```yaml
backends:
  - name: codex
    type: codex
    # base_url defaults to https://chatgpt.com/backend-api/codex
    # models:   # optional — omit to auto-discover
    #   - gpt-5.4
    #   - gpt-5.4-mini
    #   - gpt-5.3-codex
```

Available models include `gpt-5.4`, `gpt-5.4-mini`, `gpt-5.3-codex`, `gpt-5.2-codex`,
`gpt-5.2`, `gpt-5.1-codex`, `gpt-5.1-codex-max`, `gpt-5.1-codex-mini`, `gpt-5-codex`,
and `codex-mini-latest`. The default list is refreshed from the upstream API when
authentication is available; configure `model_cache_ttl` to control refresh frequency.

## Anthropic (via OpenRouter or OpenAI-compatible proxy)

Direct Anthropic API support is planned. For now, route through OpenRouter:

```yaml
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-v1-..."
```

Then use models like `openrouter/anthropic/claude-sonnet-4`.

## Self-Hosted (Ollama, vLLM, etc.)

Works with any OpenAI-compatible local server:

```yaml
backends:
  - name: local
    type: openai
    base_url: http://localhost:11434/v1 # Ollama
    api_key: "dummy" # Ollama doesn't require auth
```

```yaml
backends:
  - name: vllm
    type: openai
    base_url: http://localhost:8000/v1
    api_key: "..."
```

## Azure OpenAI

```yaml
backends:
  - name: azure
    type: openai
    base_url: https://your-resource.openai.azure.com/openai/deployments/your-deployment
    api_key: "..."
    extra_headers:
      api-key: "..."
```

## Editor Configuration

### VS Code

Install the [LLM API Proxy VS Code Extension](../llmapiproxy-vscode-extension).

Or configure manually in settings.json:

```json
{
  "github.copilot.advanced": {
    "authProvider": "AzureActiveDirectory"
  }
}
```

Note: For full integration, the extension is recommended.

### Cursor

Settings → AI → OpenAI API Key:

- API Key: Your proxy API key
- Base URL: `http://localhost:8080/v1`

### Continue.dev

`.continue/config.json`:

```json
{
  "models": [
    {
      "title": "Proxy - Claude",
      "provider": "openai",
      "model": "openrouter/anthropic/claude-sonnet-4",
      "apiBase": "http://localhost:8080/v1",
      "apiKey": "your-proxy-api-key"
    }
  ]
}
```

### Shell (aichat)

`~/.config/aichat/config.yaml`:

```yaml
clients:
  - type: openai
    api_base: http://localhost:8080/v1
    api_key: your-proxy-api-key
    models:
      - name: openrouter/gpt-4o
```
