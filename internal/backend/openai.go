package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/config"
)

// OpenAIBackend is a generic passthrough backend for any OpenAI-compatible API.
type OpenAIBackend struct {
	name         string
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	models       []config.ModelConfig
	client       *http.Client

	// modelsURL overrides the URL for model discovery (/models endpoint).
	// When empty, models are fetched from baseURL + "/models".
	modelsURL string

	// Model list cache
	modelCacheTTL time.Duration
	cacheMu       sync.RWMutex
	cachedModels  []Model
	cacheExpiry   time.Time
}

func NewOpenAI(cfg config.BackendConfig, cacheTTL time.Duration) *OpenAIBackend {
	return &OpenAIBackend{
		name:         cfg.Name,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		models:       cfg.Models,
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
		modelCacheTTL: cacheTTL,
		modelsURL:     strings.TrimRight(cfg.ModelsURL, "/"),
	}
}

func (b *OpenAIBackend) Name() string { return b.name }

func (b *OpenAIBackend) SupportsModel(modelID string) bool {
	if len(b.models) == 0 {
		// No static model list configured — verify against actual upstream catalog.
		models := b.getCachedOrFetchModels()
		if len(models) == 0 {
			return false // can't verify; don't claim support for unknown models
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

// getCachedOrFetchModels returns the cached upstream model list, fetching it
// from the upstream API if the cache is empty. Returns nil on fetch error.
func (b *OpenAIBackend) getCachedOrFetchModels() []Model {
	b.cacheMu.RLock()
	if b.cachedModels != nil {
		models := b.cachedModels
		b.cacheMu.RUnlock()
		return models
	}
	b.cacheMu.RUnlock()

	// Cache miss — fetch from upstream.
	log.Debug().Str("backend", b.name).Msg("model cache miss, fetching from upstream")
	models, err := b.buildModelList(context.Background())
	if err != nil {
		log.Error().Err(err).Str("backend", b.name).Msg("failed to fetch upstream models")
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

func (b *OpenAIBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	body := b.rewriteBody(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/chat/completions", bytes.NewReader(body))
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
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Str("body", string(errBody)).Msg("chat completion request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
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

func (b *OpenAIBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	body := b.rewriteBody(req)
	// For streaming, don't use the client timeout — the stream can last a long time.
	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/chat/completions", bytes.NewReader(body))
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
		log.Error().Str("backend", b.name).Int("status", resp.StatusCode).Str("body", string(errBody)).Msg("stream request failed")
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	return resp.Body, nil
}

// upstreamModel captures extra fields that some providers (e.g. OpenRouter) include
// in their /v1/models response.
type upstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`

	// OpenRouter-style fields
	ContextLength *int64 `json:"context_length,omitempty"`
	Architecture  *struct {
		Modality string `json:"modality"` // e.g. "text->text", "text+image->text"
	} `json:"architecture,omitempty"`
	TopProvider *struct {
		MaxCompletionTokens *int64 `json:"max_completion_tokens,omitempty"`
	} `json:"top_provider,omitempty"`

	// Standard OpenAI-compatible fields some providers expose
	MaxModelLen     *int64 `json:"max_model_len,omitempty"`
	MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
}

func (u *upstreamModel) toModel(ownerOverride string) Model {
	m := Model{
		ID:      u.ID,
		Object:  u.Object,
		Created: u.Created,
		OwnedBy: ownerOverride,
	}

	// Context window: prefer explicit context_length, fall back to max_model_len
	if u.ContextLength != nil {
		m.ContextLength = u.ContextLength
	} else if u.MaxModelLen != nil {
		m.ContextLength = u.MaxModelLen
	}

	// Max output tokens
	if u.MaxOutputTokens != nil {
		m.MaxOutputTokens = u.MaxOutputTokens
	} else if u.TopProvider != nil && u.TopProvider.MaxCompletionTokens != nil {
		m.MaxOutputTokens = u.TopProvider.MaxCompletionTokens
	}

	// Capabilities from modality (e.g. "text+image->text" → vision)
	if u.Architecture != nil {
		modality := strings.ToLower(u.Architecture.Modality)
		if strings.Contains(modality, "image") || strings.Contains(modality, "vision") {
			m.Capabilities = append(m.Capabilities, "vision")
		}
	}

	return m
}

type upstreamModelList struct {
	Object string          `json:"object"`
	Data   []upstreamModel `json:"data"`
}

func (b *OpenAIBackend) ListModels(ctx context.Context) ([]Model, error) {
	// Fast path: return cached models if still fresh.
	if b.modelCacheTTL > 0 {
		b.cacheMu.RLock()
		if !b.cacheExpiry.IsZero() && time.Now().Before(b.cacheExpiry) {
			cached := b.cachedModels
			b.cacheMu.RUnlock()
			log.Debug().Str("backend", b.name).Int("models", len(cached)).Msg("model cache hit")
			return cached, nil
		}
		b.cacheMu.RUnlock()
	}

	// Slow path: fetch from upstream and build model list.
	log.Debug().Str("backend", b.name).Msg("fetching models from upstream")
	models, err := b.buildModelList(ctx)
	if err != nil {
		// Stale-while-error: return stale cache if available.
		if b.modelCacheTTL > 0 {
			b.cacheMu.RLock()
			if b.cachedModels != nil {
				cached := b.cachedModels
				b.cacheMu.RUnlock()
				log.Warn().Err(err).Str("backend", b.name).Int("models", len(cached)).Msg("upstream fetch failed, returning stale cache")
				return cached, nil
			}
			b.cacheMu.RUnlock()
		}
		log.Error().Err(err).Str("backend", b.name).Msg("failed to fetch models")
		return nil, err
	}

	// Store in cache if caching is enabled.
	if b.modelCacheTTL > 0 {
		b.cacheMu.Lock()
		b.cachedModels = models
		b.cacheExpiry = time.Now().Add(b.modelCacheTTL)
		b.cacheMu.Unlock()
	}

	return models, nil
}

// buildModelList fetches models from upstream and builds the final list.
// This contains the original ListModels logic without caching.
func (b *OpenAIBackend) buildModelList(ctx context.Context) ([]Model, error) {
	if len(b.models) > 0 {
		// Static model list — try to enrich from upstream, ignore errors.
		upstreamMap := b.fetchUpstreamModelMap(ctx)
		seen := make(map[string]bool, len(b.models))
		models := make([]Model, 0, len(b.models))
		for _, mc := range b.models {
			if seen[mc.ID] {
				continue
			}
			seen[mc.ID] = true
			if u, ok := upstreamMap[mc.ID]; ok {
				// Upstream data found — use it, but let config overrides + known DB win.
				m := u.toModel(b.name)
				b.applyConfigOverrides(&m, mc)
				applyKnownDefaults(&m, mc.ID)
				models = append(models, m)
			} else {
				// No upstream data — build from config overrides + built-in database.
				m := Model{
					ID:      mc.ID,
					Object:  "model",
					Created: time.Now().Unix(),
					OwnedBy: b.name,
				}
				// Apply config overrides first.
				b.applyConfigOverrides(&m, mc)
				// Then fill remaining gaps from built-in database.
				applyKnownDefaults(&m, mc.ID)
				models = append(models, m)
			}
		}
		return models, nil
	}

	// Dynamic: fetch full model list from upstream.
	upstreamModels, err := b.fetchUpstreamModels(ctx)
	if err != nil {
		log.Error().Err(err).Str("backend", b.name).Msg("failed to fetch upstream models")
		return nil, err
	}
	log.Info().Str("backend", b.name).Int("count", len(upstreamModels)).Msg("fetched upstream models")
	models := make([]Model, 0, len(upstreamModels))
	for _, u := range upstreamModels {
		m := u.toModel(b.name)
		applyKnownDefaults(&m, u.ID)
		models = append(models, m)
	}
	return models, nil
}

// ClearModelCache clears the cached model list, forcing a fresh fetch on the
// next ListModels call.
func (b *OpenAIBackend) ClearModelCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cachedModels = nil
	b.cacheExpiry = time.Time{}
}

// applyConfigOverrides fills Model fields from config-level overrides when set.
func (b *OpenAIBackend) applyConfigOverrides(m *Model, mc config.ModelConfig) {
	if mc.ContextLength != nil {
		m.ContextLength = mc.ContextLength
	}
	if mc.MaxOutputTokens != nil {
		m.MaxOutputTokens = mc.MaxOutputTokens
	}
}

// modelsEndpoint returns the URL for the /models endpoint.
// If modelsURL is configured, it takes precedence over baseURL + "/models".
func (b *OpenAIBackend) modelsEndpoint() string {
	if b.modelsURL != "" {
		return b.modelsURL + "/models"
	}
	return b.baseURL + "/models"
}

// fetchUpstreamModels fetches and returns the raw upstream model list.
func (b *OpenAIBackend) fetchUpstreamModels(ctx context.Context) ([]upstreamModel, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.modelsEndpoint(), nil)
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
		return nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
	}

	var list upstreamModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding models: %w", err)
	}
	return list.Data, nil
}

// fetchUpstreamModelMap returns a map of model ID → upstream model for enrichment.
// Errors are silently ignored — it is only used as a best-effort enrichment.
func (b *OpenAIBackend) fetchUpstreamModelMap(ctx context.Context) map[string]upstreamModel {
	models, _ := b.fetchUpstreamModels(ctx)
	m := make(map[string]upstreamModel, len(models))
	for _, u := range models {
		m[u.ID] = u
	}
	return m
}

// FetchUpstreamModelsRaw returns the raw HTTP response body from the upstream models endpoint.
func (b *OpenAIBackend) FetchUpstreamModelsRaw(ctx context.Context) (*UpstreamModelsResponse, error) {
	modelsURL := b.modelsEndpoint()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, "")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return &UpstreamModelsResponse{
			Backend:    b.name,
			URL:        modelsURL,
			StatusCode: 0,
			Error:      fmt.Sprintf("fetch error: %v", err),
		}, nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := &UpstreamModelsResponse{
		Backend:     b.name,
		URL:         modelsURL,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		RawBody:     string(body),
	}
	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return result, nil
}

func (b *OpenAIBackend) setHeaders(httpReq *http.Request, apiKeyOverride string) {
	apiKey := b.apiKey
	if apiKeyOverride != "" {
		apiKey = apiKeyOverride
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range b.extraHeaders {
		httpReq.Header.Set(k, v)
	}
}

func (b *OpenAIBackend) rewriteBody(req *ChatCompletionRequest) []byte {
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

// RewriteResponseBody rewrites only the "model" field in a raw response JSON,
// preserving all other fields (tool_calls, usage details, system_fingerprint, etc.)
// for transparent passthrough.
func RewriteResponseBody(rawBody []byte, newModel string) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &m); err != nil {
		return rawBody
	}
	modelBytes, _ := json.Marshal(newModel)
	m["model"] = modelBytes
	data, _ := json.Marshal(m)
	return data
}
