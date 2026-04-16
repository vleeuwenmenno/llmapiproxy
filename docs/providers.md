# Provider Setup Guides

LLM API Proxy works with any provider implementing the OpenAI or Anthropic API format. Below are setup guides for popular providers.

---

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

Models are referenced with their full provider path:

```
openrouter/openai/gpt-4o
openrouter/anthropic/claude-sonnet-4
openrouter/google/gemini-2.5-pro
openrouter/meta-llama/llama-4-maverick
```

Get your API key at [openrouter.ai/keys](https://openrouter.ai/keys).

---

## Z.ai

[z.ai](https://z.ai) — GLM models from Zhipu AI. Offers both general-purpose and coding-specific endpoints.

### General API

```yaml
backends:
  - name: zai
    type: openai
    base_url: https://api.z.ai/api/paas/v4
    api_key: "your-zai-api-key"
    models:
      - glm-5.1
      - glm-5-turbo
      - glm-4.7
      - glm-4.5-air
```

### Coding Plan

Same API key, dedicated endpoint for coding subscription quota:

```yaml
backends:
  - name: zai-coding
    type: openai
    base_url: https://api.z.ai/api/coding/paas/v4
    api_key: "your-zai-api-key"
    models:
      - glm-5.1
      - glm-5-turbo
      - glm-4.7
```

Get your API key at [z.ai/manage-apikey](https://z.ai/manage-apikey/apikey-list).

---

## OpenCode (Zen & Go)

[opencode.ai](https://opencode.ai) — Curated coding models.

### Zen (Pay-as-you-go)

```yaml
backends:
  - name: zen
    type: openai
    base_url: https://opencode.ai/zen/v1
    api_key: "your-opencode-key"
    models:
      - minimax-m2.5
      - glm-5.1
      - kimi-k2.5
      - big-pickle
```

### Go (Subscription)

Low-cost subscription ($5 first month, $10/month). Uses the same API key as Zen.

> **Note:** The Go endpoint does not expose a `/models` endpoint (returns 404). Use `models_url` to point to the Zen endpoint for model discovery while routing completions to Go:

```yaml
backends:
  - name: go
    type: openai
    base_url: https://opencode.ai/zen/go/v1
    api_key: "your-opencode-key"
    models_url: https://opencode.ai/zen/v1
```

Get your API key at [opencode.ai/auth](https://opencode.ai/auth).

---

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
      - gpt-5.2
```

---

## Anthropic (Direct)

The `anthropic` backend type connects directly to the Anthropic Messages API:

```yaml
backends:
  - name: anthropic
    type: anthropic
    base_url: https://api.anthropic.com/v1
    api_key: "sk-ant-..."
    extra_headers:
      anthropic-version: "2023-06-01"
```

Models: `anthropic/claude-sonnet-4`, `anthropic/claude-opus-4`, `anthropic/claude-haiku-4`, etc.

The proxy also accepts Anthropic-style requests on `/v1/messages` for any backend — see the [API Reference](api.md) for details.

---

## GitHub Copilot

Uses GitHub Device Code Flow for authentication. No API key needed — authenticate through the web UI.

```yaml
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
    # No api_key needed — authentication uses Device Code Flow
```

**Prerequisites:** A GitHub account with Copilot access (Individual, Business, or Enterprise).

**To authenticate:**

1. Navigate to **Settings** in the web dashboard (`/ui/settings`)
2. Find the Copilot backend in the OAuth section
3. Click **Connect** to start the Device Code Flow
4. Visit the verification URL and enter the displayed code
5. Authorize the application — the proxy stores the token automatically

Models: `copilot/gpt-4o`, `copilot/gpt-4.1`, `copilot/o3`, `copilot/o4-mini`, etc. Models are dynamically fetched from the Copilot API.

---

## OpenAI Codex

Uses OAuth PKCE flow for authentication. No API key needed.

```yaml
backends:
  - name: codex
    type: codex
    # base_url defaults to https://chatgpt.com/backend-api/codex
    oauth:
      scopes:
        - "openid"
        - "profile"
        - "email"
        - "offline_access"
      auth_url: "https://auth.openai.com/oauth/authorize"
      token_url: "https://auth.openai.com/oauth/token"
```

**To authenticate:**

1. Navigate to **Settings** in the web dashboard (`/ui/settings`)
2. Find the Codex backend in the OAuth section
3. Click **Connect** to start the OAuth login flow
4. Complete authentication in the browser — the proxy stores the token

Tokens are persisted to disk and refreshed automatically. Models are dynamically fetched when authenticated.

Codex backends also support the native `/v1/responses` endpoint for passthrough to the OpenAI Responses API.

---

## Self-Hosted (Ollama, vLLM, etc.)

Works with any OpenAI-compatible local server:

### Ollama

```yaml
backends:
  - name: local
    type: openai
    base_url: http://localhost:11434/v1
    api_key: "dummy" # Ollama doesn't require auth
```

### vLLM

```yaml
backends:
  - name: vllm
    type: openai
    base_url: http://localhost:8000/v1
    api_key: "..."
```

### LM Studio

```yaml
backends:
  - name: lmstudio
    type: openai
    base_url: http://localhost:1234/v1
    api_key: "dummy"
```

---

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

---

## NVIDIA NIM

```yaml
backends:
  - name: nvidia
    type: openai
    base_url: https://integrate.api.nvidia.com/v1
    api_key: "nvapi-..."
```
