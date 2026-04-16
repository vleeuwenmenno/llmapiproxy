package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/identity"
	"github.com/rs/zerolog/log"
)

const ollamaHTTPTimeout = 5 * time.Minute

// OllamaBackend implements the Backend interface for Ollama.
// It supports three compatibility modes controlled by config.BackendConfig.CompatMode:
//   - "openai" (default): routes to /v1/chat/completions (OpenAI-compatible)
//   - "anthropic": routes to /v1/messages (Anthropic-compatible)
//   - "native": routes to /api/chat (Ollama native format)
//
// Model listing always uses /api/tags. Model management (pull, delete, show)
// always uses Ollama's native API regardless of compat mode.
type OllamaBackend struct {
	name         string
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	models       []config.ModelConfig
	client       *http.Client

	identityProfile *identity.Profile
	disabledModels  map[string]bool
	compatMode      string // "openai", "anthropic", or "native"

	// Model list cache
	modelCacheTTL time.Duration
	cacheMu       sync.RWMutex
	cachedModels  []Model
	cacheExpiry   time.Time
	cacheStore    *ModelCacheStore

	// Pull tracking — survives page reloads since the backend instance is long-lived.
	pullMu    sync.RWMutex
	pulls     map[string]*OllamaPullStatus  // keyed by pull ID
	cancelFns map[string]context.CancelFunc // pull ID → cancel function
}

func NewOllama(cfg config.BackendConfig, cacheTTL time.Duration, profile *identity.Profile) *OllamaBackend {
	dm := make(map[string]bool, len(cfg.DisabledModels))
	for _, m := range cfg.DisabledModels {
		dm[m] = true
	}
	compatMode := cfg.CompatMode
	if compatMode == "" {
		compatMode = "openai"
	}
	return &OllamaBackend{
		name:            cfg.Name,
		baseURL:         strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:          cfg.APIKey,
		extraHeaders:    cfg.ExtraHeaders,
		models:          cfg.Models,
		client:          &http.Client{Timeout: ollamaHTTPTimeout},
		modelCacheTTL:   cacheTTL,
		disabledModels:  dm,
		identityProfile: profile,
		compatMode:      compatMode,
		pulls:           make(map[string]*OllamaPullStatus),
		cancelFns:       make(map[string]context.CancelFunc),
	}
}

// CompatMode returns the current compatibility mode.
func (b *OllamaBackend) CompatMode() string { return b.compatMode }

// SetCompatMode changes the compatibility mode (used by web UI switch-type).
func (b *OllamaBackend) SetCompatMode(mode string) { b.compatMode = mode }

func (b *OllamaBackend) Name() string { return b.name }

func (b *OllamaBackend) SetModelCacheStore(store *ModelCacheStore) {
	b.cacheStore = store
}

func (b *OllamaBackend) SupportsModel(modelID string) bool {
	if b.disabledModels[modelID] {
		return false
	}
	if len(b.models) == 0 {
		models := b.getCachedOrFetchModels()
		if len(models) == 0 {
			return false
		}
		for _, m := range models {
			if m.ID == modelID {
				return true
			}
		}
		return false
	}
	for _, m := range b.models {
		if m.ID == modelID {
			return true
		}
		if strings.HasSuffix(m.ID, "/*") {
			prefix := strings.TrimSuffix(m.ID, "/*")
			if strings.HasPrefix(modelID, prefix+"/") || modelID == prefix {
				return true
			}
		}
	}
	return false
}

func (b *OllamaBackend) ResolveModelID(canonicalID string) string {
	for _, m := range b.models {
		if m.ID == canonicalID {
			return canonicalID
		}
		if lastSegment(m.ID) == canonicalID {
			return m.ID
		}
	}
	for _, m := range b.getCachedOrFetchModels() {
		if m.ID == canonicalID {
			return canonicalID
		}
		if lastSegment(m.ID) == canonicalID {
			return m.ID
		}
	}
	return canonicalID
}

// ── ChatCompletion (non-streaming) ──────────────────────────

func (b *OllamaBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	switch b.compatMode {
	case "anthropic":
		return b.chatCompletionAnthropic(ctx, req)
	case "native":
		return b.chatCompletionNative(ctx, req)
	default: // "openai"
		return b.chatCompletionOpenAI(ctx, req)
	}
}

// ── ChatCompletionStream ────────────────────────────────────

func (b *OllamaBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	switch b.compatMode {
	case "anthropic":
		return b.chatStreamAnthropic(ctx, req)
	case "native":
		return b.chatStreamNative(ctx, req)
	default: // "openai"
		return b.chatStreamOpenAI(ctx, req)
	}
}

// ── OpenAI compat mode ──────────────────────────────────────

func (b *OllamaBackend) chatCompletionOpenAI(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	body := b.rewriteBodyOpenAI(req)
	endpoint := b.baseURL + "/v1/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Str("body", string(errBody)).Msg("ollama openai chat request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var result ChatCompletionResponse
	if err := json.Unmarshal(rawBody, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	result.RawBody = rawBody
	return &result, nil
}

func (b *OllamaBackend) chatStreamOpenAI(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	body := b.rewriteBodyOpenAI(req)
	endpoint := b.baseURL + "/v1/chat/completions"

	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Msg("ollama openai stream request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	// Ollama's /v1/chat/completions returns standard SSE — pass through directly.
	return resp.Body, nil
}

// ── Anthropic compat mode ────────────────────────────────────

func (b *OllamaBackend) chatCompletionAnthropic(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	// Convert OpenAI format to Anthropic format, send to Ollama's /v1/messages,
	// then convert the Anthropic response back to OpenAI format.
	anthReq, err := b.convertToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("converting to anthropic format: %w", err)
	}
	anthReq.Stream = false

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := b.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	// Try to parse as Anthropic response first.
	var anthropicResp struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(rawBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("decoding anthropic response: %w", err)
	}

	// Convert Anthropic response to OpenAI format.
	var contentText string
	for _, block := range anthropicResp.Content {
		if block.Text != "" {
			contentText += block.Text
		}
	}
	contentBytes, _ := json.Marshal(contentText)
	finishReason := "stop"
	if anthropicResp.StopReason == "max_tokens" {
		finishReason = "length"
	}

	chatResp := &ChatCompletionResponse{
		ID:      anthropicResp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   anthropicResp.Model,
		Choices: []Choice{{
			Index:        0,
			Message:      &Message{Role: "assistant", Content: contentBytes},
			FinishReason: &finishReason,
		}},
	}
	if anthropicResp.Usage != nil {
		chatResp.Usage = &Usage{
			PromptTokens:     anthropicResp.Usage.InputTokens,
			CompletionTokens: anthropicResp.Usage.OutputTokens,
			TotalTokens:      anthropicResp.Usage.InputTokens + anthropicResp.Usage.OutputTokens,
		}
	}
	chatResp.RawBody = rawBody
	return chatResp, nil
}

func (b *OllamaBackend) chatStreamAnthropic(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	anthReq, err := b.convertToAnthropicRequest(req)
	if err != nil {
		return nil, fmt.Errorf("converting to anthropic format: %w", err)
	}
	anthReq.Stream = true

	body, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	endpoint := b.baseURL + "/v1/messages"
	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	// Ollama's /v1/messages returns Anthropic SSE — wrap to convert to OpenAI SSE.
	return newAnthropicStreamReader(resp.Body), nil
}

// ── Native mode ─────────────────────────────────────────────

func (b *OllamaBackend) chatCompletionNative(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	ollamaReq, err := b.convertRequest(req)
	if err != nil {
		return nil, fmt.Errorf("converting request: %w", err)
	}
	ollamaReq.Stream = false

	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Str("body", string(errBody)).Msg("ollama chat request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	// Ollama non-streaming returns a single JSON object
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	chatResp := ollamaResponseToChatCompletion(rawBody)
	chatResp.RawBody = rawBody
	return chatResp, nil
}

// ── Native mode stream ─────────────────────────────────────

func (b *OllamaBackend) chatStreamNative(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	ollamaReq, err := b.convertRequest(req)
	if err != nil {
		return nil, fmt.Errorf("converting request: %w", err)
	}
	ollamaReq.Stream = true

	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	client := &http.Client{} // No timeout for streaming
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Str("body", string(errBody)).Msg("ollama stream request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("ollama %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	return newOllamaStreamReader(resp.Body, req.Model), nil
}

// ── OpenAI body rewrite ─────────────────────────────────────

// rewriteBodyOpenAI rewrites the request body for OpenAI-compatible endpoint,
// updating the model field to use the resolved model ID.
func (b *OllamaBackend) rewriteBodyOpenAI(req *ChatCompletionRequest) []byte {
	if len(req.RawBody) == 0 {
		data, _ := json.Marshal(req)
		return data
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(req.RawBody, &m); err != nil {
		data, _ := json.Marshal(req)
		return data
	}

	modelBytes, _ := json.Marshal(req.Model)
	m["model"] = modelBytes
	data, _ := json.Marshal(m)
	return data
}

// ── Anthropic request conversion ────────────────────────────

type ollamaAnthropicRequest struct {
	Model       string                   `json:"model"`
	Messages    []ollamaAnthropicMessage `json:"messages"`
	System      string                   `json:"system,omitempty"`
	MaxTokens   int                      `json:"max_tokens"`
	Stream      bool                     `json:"stream,omitempty"`
	Temperature *float64                 `json:"temperature,omitempty"`
	TopP        *float64                 `json:"top_p,omitempty"`
}

type ollamaAnthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (b *OllamaBackend) convertToAnthropicRequest(req *ChatCompletionRequest) (*ollamaAnthropicRequest, error) {
	maxTokens := 1024
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	}

	var raw map[string]json.RawMessage
	if len(req.RawBody) > 0 {
		_ = json.Unmarshal(req.RawBody, &raw)
	}

	systemParts := make([]string, 0)
	messages := make([]ollamaAnthropicMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		text := chatContentToText(msg.Content)
		switch msg.Role {
		case "system", "developer":
			if strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}
		default:
			if strings.TrimSpace(text) == "" {
				continue
			}
			messages = append(messages, ollamaAnthropicMessage{
				Role:    msg.Role,
				Content: text,
			})
		}
	}

	out := &ollamaAnthropicRequest{
		Model:     req.Model,
		Messages:  messages,
		System:    strings.Join(systemParts, "\n\n"),
		MaxTokens: maxTokens,
		Stream:    req.Stream,
	}

	if req.Temperature != nil {
		out.Temperature = req.Temperature
	}
	if raw != nil {
		if topPRaw, ok := raw["top_p"]; ok {
			var topP float64
			if json.Unmarshal(topPRaw, &topP) == nil {
				out.TopP = &topP
			}
		}
	}

	return out, nil
}

// ── Model listing (GET /api/tags) ───────────────────────────

type ollamaTagsResponse struct {
	Models []ollamaTagModel `json:"models"`
}

type ollamaTagModel struct {
	Name       string             `json:"name"`
	Model      string             `json:"model"`
	ModifiedAt string             `json:"modified_at"`
	Size       int64              `json:"size"`
	Digest     string             `json:"digest"`
	Details    ollamaModelDetails `json:"details"`
}

type ollamaModelDetails struct {
	ParentModel       string   `json:"parent_model"`
	Format            string   `json:"format"`
	Family            string   `json:"family"`
	Families          []string `json:"families"`
	ParameterSize     string   `json:"parameter_size"`
	QuantizationLevel string   `json:"quantization_level"`
}

func (b *OllamaBackend) ListModels(ctx context.Context) ([]Model, error) {
	if b.modelCacheTTL > 0 {
		b.cacheMu.RLock()
		if !b.cacheExpiry.IsZero() && time.Now().Before(b.cacheExpiry) {
			cached := b.cachedModels
			b.cacheMu.RUnlock()
			log.Debug().Str("backend", b.name).Int("models", len(cached)).Msg("ollama model cache hit")
			return b.markDisabled(cached), nil
		}
		b.cacheMu.RUnlock()

		if b.cacheStore != nil {
			if models, expiry, ok := b.cacheStore.Load(b.name); ok && len(models) > 0 && time.Now().Before(expiry) {
				b.cacheMu.Lock()
				b.cachedModels = models
				b.cacheExpiry = expiry
				b.cacheMu.Unlock()
				log.Debug().Str("backend", b.name).Int("models", len(models)).Msg("ollama model cache restored from disk")
				return b.markDisabled(models), nil
			}
		}
	}

	log.Debug().Str("backend", b.name).Msg("ollama fetching models from /api/tags")
	models, err := b.buildModelList(ctx)
	if err != nil {
		if b.modelCacheTTL > 0 {
			if b.cacheStore != nil {
				if diskModels, _, ok := b.cacheStore.Load(b.name); ok && len(diskModels) > 0 {
					log.Warn().Err(err).Str("backend", b.name).Int("models", len(diskModels)).Msg("ollama upstream fetch failed, returning disk cache")
					return b.markDisabled(diskModels), nil
				}
			}
			b.cacheMu.RLock()
			if b.cachedModels != nil {
				cached := b.cachedModels
				b.cacheMu.RUnlock()
				log.Warn().Err(err).Str("backend", b.name).Int("models", len(cached)).Msg("ollama upstream fetch failed, returning stale cache")
				return b.markDisabled(cached), nil
			}
			b.cacheMu.RUnlock()
		}
		return nil, err
	}

	if b.modelCacheTTL > 0 {
		expiry := time.Now().Add(b.modelCacheTTL)
		b.cacheMu.Lock()
		b.cachedModels = models
		b.cacheExpiry = expiry
		b.cacheMu.Unlock()
		if b.cacheStore != nil {
			b.cacheStore.Save(b.name, models, expiry)
		}
	}

	return b.markDisabled(models), nil
}

func (b *OllamaBackend) buildModelList(ctx context.Context) ([]Model, error) {
	// If static models configured, use those enriched with upstream data.
	if len(b.models) > 0 {
		upstreamMap := b.fetchOllamaModelsMap(ctx)
		seen := make(map[string]bool, len(b.models))
		models := make([]Model, 0, len(b.models))
		for _, mc := range b.models {
			if seen[mc.ID] {
				continue
			}
			seen[mc.ID] = true
			m := Model{
				ID:      mc.ID,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: b.name,
			}
			if u, ok := upstreamMap[mc.ID]; ok {
				m.Created = parseOllamaTime(u.ModifiedAt)
				m.OwnedBy = "library"
				if u.Details.Family != "" {
					m.DisplayName = u.Details.ParameterSize + " " + u.Details.Family
				}
				if u.Details.Families != nil {
					for _, fam := range u.Details.Families {
						if strings.Contains(strings.ToLower(fam), "clip") || strings.Contains(strings.ToLower(fam), "vision") {
							m.Capabilities = append(m.Capabilities, "vision")
						}
					}
				}
			}
			b.applyConfigOverrides(&m, mc)
			applyKnownDefaults(&m, mc.ID)
			models = append(models, m)
		}
		return models, nil
	}

	// Dynamic: fetch from /api/tags.
	ollamaModels, err := b.fetchOllamaModels(ctx)
	if err != nil {
		return nil, err
	}
	log.Info().Str("backend", b.name).Int("count", len(ollamaModels)).Msg("ollama fetched models")
	models := make([]Model, 0, len(ollamaModels))
	for _, u := range ollamaModels {
		m := Model{
			ID:      u.Name,
			Object:  "model",
			Created: parseOllamaTime(u.ModifiedAt),
			OwnedBy: "library",
		}
		if u.Details.Family != "" {
			m.DisplayName = u.Details.ParameterSize + " " + u.Details.Family
		}
		if u.Details.Families != nil {
			for _, fam := range u.Details.Families {
				if strings.Contains(strings.ToLower(fam), "clip") || strings.Contains(strings.ToLower(fam), "vision") {
					m.Capabilities = append(m.Capabilities, "vision")
				}
			}
		}
		applyKnownDefaults(&m, u.Name)
		models = append(models, m)
	}
	return models, nil
}

func (b *OllamaBackend) fetchOllamaModels(ctx context.Context) ([]ollamaTagModel, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, "")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var tags ollamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decoding models: %w", err)
	}
	return tags.Models, nil
}

func (b *OllamaBackend) fetchOllamaModelsMap(ctx context.Context) map[string]ollamaTagModel {
	models, _ := b.fetchOllamaModels(ctx)
	m := make(map[string]ollamaTagModel, len(models))
	for _, u := range models {
		m[u.Name] = u
	}
	return m
}

func (b *OllamaBackend) getCachedOrFetchModels() []Model {
	b.cacheMu.RLock()
	if b.cachedModels != nil {
		models := b.cachedModels
		b.cacheMu.RUnlock()
		return models
	}
	b.cacheMu.RUnlock()

	if b.cacheStore != nil {
		if models, _, ok := b.cacheStore.Load(b.name); ok && len(models) > 0 {
			b.cacheMu.Lock()
			if b.cachedModels == nil {
				b.cachedModels = models
			}
			b.cacheMu.Unlock()
			return models
		}
	}

	models, err := b.buildModelList(context.Background())
	if err != nil {
		log.Error().Err(err).Str("backend", b.name).Msg("ollama failed to fetch models")
		return nil
	}

	b.cacheMu.Lock()
	if b.cachedModels == nil {
		b.cachedModels = models
		if b.modelCacheTTL > 0 {
			b.cacheExpiry = time.Now().Add(b.modelCacheTTL)
		}
	}
	b.cacheMu.Unlock()
	return models
}

func (b *OllamaBackend) ClearModelCache() {
	b.cacheMu.Lock()
	b.cachedModels = nil
	b.cacheExpiry = time.Time{}
	b.cacheMu.Unlock()
	// Also clear disk cache so next ListModels fetches from upstream.
	if b.cacheStore != nil {
		b.cacheStore.Invalidate(b.name)
	}
}

// ActivePulls returns snapshots of pulls that are still in progress or have
// errors (to surface them in the UI). Completed successful pulls are removed.
func (b *OllamaBackend) ActivePulls() []OllamaPullStatus {
	b.pullMu.Lock()
	defer b.pullMu.Unlock()
	out := make([]OllamaPullStatus, 0, len(b.pulls))
	for id, s := range b.pulls {
		if s.Progress.Done && s.Progress.Error == "" {
			// Completed successfully — clean up.
			delete(b.pulls, id)
			delete(b.cancelFns, id)
			continue
		}
		out = append(out, *s)
	}
	return out
}

// StorePull stores a pull status entry.
func (b *OllamaBackend) StorePull(pullID string, status *OllamaPullStatus) {
	b.pullMu.Lock()
	b.pulls[pullID] = status
	b.pullMu.Unlock()
}

// StoreCancel stores the cancel function for a pull.
func (b *OllamaBackend) StoreCancel(pullID string, cancel context.CancelFunc) {
	b.pullMu.Lock()
	b.cancelFns[pullID] = cancel
	b.pullMu.Unlock()
}

// UpdatePullProgress updates the progress of a pull.
func (b *OllamaBackend) UpdatePullProgress(pullID string, progress OllamaPullProgress) {
	b.pullMu.Lock()
	if s, ok := b.pulls[pullID]; ok {
		s.Progress = progress
	}
	b.pullMu.Unlock()
}

// DeletePull removes a pull entry and its cancel function.
func (b *OllamaBackend) DeletePull(pullID string) {
	b.pullMu.Lock()
	delete(b.pulls, pullID)
	delete(b.cancelFns, pullID)
	b.pullMu.Unlock()
}

// CancelPull cancels an active pull by its ID. Returns false if not found.
func (b *OllamaBackend) CancelPull(pullID string) bool {
	b.pullMu.Lock()
	cancel, ok := b.cancelFns[pullID]
	if ok {
		cancel()
		// Mark the pull as cancelled.
		if s, sok := b.pulls[pullID]; sok {
			s.Progress = OllamaPullProgress{Status: "cancelled", Done: true}
		}
	}
	b.pullMu.Unlock()
	return ok
}

// CancelPullByModel cancels any active pull for the given model name.
func (b *OllamaBackend) CancelPullByModel(modelName string) bool {
	b.pullMu.Lock()
	defer b.pullMu.Unlock()
	for id, s := range b.pulls {
		if s.ModelName == modelName && !s.Progress.Done {
			if cancel, ok := b.cancelFns[id]; ok {
				cancel()
				s.Progress = OllamaPullProgress{Status: "cancelled", Done: true}
				return true
			}
		}
	}
	return false
}

func (b *OllamaBackend) markDisabled(models []Model) []Model {
	if len(b.disabledModels) == 0 {
		return models
	}
	for i := range models {
		if b.disabledModels[models[i].ID] {
			models[i].Disabled = true
		}
	}
	return models
}

func (b *OllamaBackend) applyConfigOverrides(m *Model, mc config.ModelConfig) {
	if mc.ContextLength != nil {
		m.ContextLength = mc.ContextLength
	}
	if mc.MaxOutputTokens != nil {
		m.MaxOutputTokens = mc.MaxOutputTokens
	}
}

// FetchUpstreamModelsRaw implements UpstreamModelsProvider for debugging.
func (b *OllamaBackend) FetchUpstreamModelsRaw(ctx context.Context) (*UpstreamModelsResponse, error) {
	url := b.baseURL + "/api/tags"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, "")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return &UpstreamModelsResponse{
			Backend:    b.name,
			URL:        url,
			StatusCode: 0,
			Error:      fmt.Sprintf("fetch error: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := &UpstreamModelsResponse{
		Backend:     b.name,
		URL:         url,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		RawBody:     string(body),
	}
	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return result, nil
}

// ── Request conversion: OpenAI → Ollama /api/chat ───────────

type ollamaChatRequest struct {
	Model    string              `json:"model"`
	Messages []ollamaChatMessage `json:"messages"`
	Stream   bool                `json:"stream"`
	Format   interface{}         `json:"format,omitempty"`
	Options  *ollamaOptions      `json:"options,omitempty"`
}

type ollamaChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"` // equivalent to max_tokens
	Stop        []string `json:"stop,omitempty"`
}

func (b *OllamaBackend) convertRequest(req *ChatCompletionRequest) (*ollamaChatRequest, error) {
	messages := make([]ollamaChatMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		text := chatContentToText(msg.Content)
		messages = append(messages, ollamaChatMessage{
			Role:    msg.Role,
			Content: text,
		})
	}

	ollamaReq := &ollamaChatRequest{
		Model:    req.Model,
		Messages: messages,
		Stream:   req.Stream,
	}

	// Build options from request parameters.
	opts := &ollamaOptions{}
	if req.Temperature != nil {
		opts.Temperature = req.Temperature
	}
	if req.MaxTokens != nil {
		opts.NumPredict = req.MaxTokens
	}

	// Extract additional parameters from raw body.
	if len(req.RawBody) > 0 {
		var raw map[string]json.RawMessage
		if json.Unmarshal(req.RawBody, &raw) == nil {
			if topPRaw, ok := raw["top_p"]; ok {
				var topP float64
				if json.Unmarshal(topPRaw, &topP) == nil {
					opts.TopP = &topP
				}
			}
			if stopRaw, ok := raw["stop"]; ok {
				var stops []string
				if json.Unmarshal(stopRaw, &stops) == nil && len(stops) > 0 {
					opts.Stop = stops
				} else {
					var single string
					if json.Unmarshal(stopRaw, &single) == nil && single != "" {
						opts.Stop = []string{single}
					}
				}
			}
		}
	}

	ollamaReq.Options = opts
	return ollamaReq, nil
}

// ── Response conversion: Ollama → OpenAI ────────────────────

type ollamaChatResponse struct {
	Model     string `json:"model"`
	CreatedAt string `json:"created_at"`
	Message   struct {
		Role     string `json:"role"`
		Content  string `json:"content"`
		Thinking string `json:"thinking,omitempty"`
	} `json:"message"`
	Done               bool   `json:"done"`
	DoneReason         string `json:"done_reason,omitempty"`
	TotalDuration      int64  `json:"total_duration,omitempty"`
	LoadDuration       int64  `json:"load_duration,omitempty"`
	PromptEvalCount    int    `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64  `json:"prompt_eval_duration,omitempty"`
	EvalCount          int    `json:"eval_count,omitempty"`
	EvalDuration       int64  `json:"eval_duration,omitempty"`
}

func ollamaResponseToChatCompletion(raw []byte) *ChatCompletionResponse {
	var ollamaResp ollamaChatResponse
	if err := json.Unmarshal(raw, &ollamaResp); err != nil {
		// Return a minimal response on parse failure.
		return &ChatCompletionResponse{
			Object:  "chat.completion",
			Created: time.Now().Unix(),
			Choices: []Choice{{
				Index: 0,
				Message: &Message{
					Role:    "assistant",
					Content: json.RawMessage(`""`),
				},
			}},
		}
	}

	content, _ := json.Marshal(ollamaResp.Message.Content)
	finishReason := "stop"
	if ollamaResp.DoneReason == "length" {
		finishReason = "length"
	}

	resp := &ChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.New().String()[:8],
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   ollamaResp.Model,
		Choices: []Choice{
			{
				Index:        0,
				Message:      &Message{Role: "assistant", Content: content},
				FinishReason: &finishReason,
			},
		},
	}

	if ollamaResp.PromptEvalCount > 0 || ollamaResp.EvalCount > 0 {
		resp.Usage = &Usage{
			PromptTokens:     ollamaResp.PromptEvalCount,
			CompletionTokens: ollamaResp.EvalCount,
			TotalTokens:      ollamaResp.PromptEvalCount + ollamaResp.EvalCount,
		}
	}

	return resp
}

// ── Streaming: Ollama NDJSON → SSE ──────────────────────────

// ollamaStreamReader translates Ollama's streaming NDJSON format
// (one JSON object per line with "done" field) into OpenAI SSE format.
type ollamaStreamReader struct {
	source     io.ReadCloser
	scanner    *bufio.Scanner
	responseID string
	modelName  string
	buf        bytes.Buffer
	done       bool
	sentFirst  bool
}

func newOllamaStreamReader(source io.ReadCloser, modelName string) *ollamaStreamReader {
	s := &ollamaStreamReader{
		source:     source,
		responseID: "chatcmpl-" + uuid.New().String()[:8],
		modelName:  modelName,
	}
	s.scanner = bufio.NewScanner(source)
	s.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return s
}

func (r *ollamaStreamReader) Read(p []byte) (int, error) {
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	if r.done {
		return 0, io.EOF
	}

	for r.buf.Len() == 0 && !r.done {
		if !r.scanner.Scan() {
			r.done = true
			r.writeSSE("data", "[DONE]\n\n")
			return r.buf.Read(p)
		}

		line := r.scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		var chunk ollamaChatResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			log.Debug().Str("line", line).Msg("ollama stream: skipping unparseable line")
			continue
		}

		if !r.sentFirst {
			// Send role-only first chunk.
			r.sentFirst = true
			roleData, _ := json.Marshal(map[string]any{
				"id":      r.responseID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   r.modelName,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{"role": "assistant", "content": ""},
					"finish_reason": nil,
				}},
			})
			r.writeSSE("data", string(roleData)+"\n\n")
		}

		if chunk.Done {
			// Final chunk — send finish reason.
			finishReason := "stop"
			if chunk.DoneReason == "length" {
				finishReason = "length"
			}
			data, _ := json.Marshal(map[string]any{
				"id":      r.responseID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   r.modelName,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{},
					"finish_reason": finishReason,
				}},
			})
			r.writeSSE("data", string(data)+"\n\n")
			r.writeSSE("data", "[DONE]\n\n")
			r.done = true
			continue
		}

		// Content chunk.
		if chunk.Message.Content != "" {
			data, _ := json.Marshal(map[string]any{
				"id":      r.responseID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   r.modelName,
				"choices": []map[string]any{{
					"index":         0,
					"delta":         map[string]any{"content": chunk.Message.Content},
					"finish_reason": nil,
				}},
			})
			r.writeSSE("data", string(data)+"\n\n")
		}
	}

	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	return 0, io.EOF
}

func (r *ollamaStreamReader) writeSSE(field, data string) {
	r.buf.WriteString(field + ": " + data)
}

func (r *ollamaStreamReader) Close() error {
	return r.source.Close()
}

// ── Helpers ─────────────────────────────────────────────────

func (b *OllamaBackend) setHeaders(httpReq *http.Request, apiKeyOverride string) {
	httpReq.Header.Set("Content-Type", "application/json")
	apiKey := b.apiKey
	if apiKeyOverride != "" {
		apiKey = apiKeyOverride
	}
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	identity.ApplyProfile(httpReq, b.identityProfile, "")

	for k, v := range b.extraHeaders {
		httpReq.Header.Set(k, v)
	}
}

// parseOllamaTime parses an ISO 8601 timestamp from Ollama's API.
func parseOllamaTime(s string) int64 {
	if s == "" {
		return time.Now().Unix()
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// Try without nano.
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return time.Now().Unix()
		}
	}
	return t.Unix()
}
