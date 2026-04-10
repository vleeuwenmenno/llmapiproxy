package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
}

func NewOpenAI(cfg config.BackendConfig) *OpenAIBackend {
	return &OpenAIBackend{
		name:         cfg.Name,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		models:       cfg.Models,
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

func (b *OpenAIBackend) Name() string { return b.name }

func (b *OpenAIBackend) SupportsModel(modelID string) bool {
	if len(b.models) == 0 {
		return true
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
		return nil, &BackendError{StatusCode: resp.StatusCode, Body: string(errBody), Err: fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))}
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
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
	if len(b.models) > 0 {
		// Static model list — try to enrich from upstream, ignore errors.
		upstreamMap := b.fetchUpstreamModelMap(ctx)
		models := make([]Model, 0, len(b.models))
		for _, mc := range b.models {
			if u, ok := upstreamMap[mc.ID]; ok {
				// Upstream data found — use it, but let config overrides + known DB win.
				m := u.toModel(b.name)
				b.applyConfigOverrides(&m, mc)
				b.applyKnownDefaults(&m, mc.ID)
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
				b.applyKnownDefaults(&m, mc.ID)
				models = append(models, m)
			}
		}
		return models, nil
	}

	// Dynamic: fetch full model list from upstream.
	upstreamModels, err := b.fetchUpstreamModels(ctx)
	if err != nil {
		return nil, err
	}
	models := make([]Model, 0, len(upstreamModels))
	for _, u := range upstreamModels {
		m := u.toModel(b.name)
		b.applyKnownDefaults(&m, u.ID)
		models = append(models, m)
	}
	return models, nil
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

// applyKnownDefaults fills missing Model fields from the built-in model database.
func (b *OpenAIBackend) applyKnownDefaults(m *Model, modelID string) {
	info := LookupKnownModel(modelID)
	if info == nil {
		return
	}
	if m.DisplayName == "" && info.DisplayName != "" {
		m.DisplayName = info.DisplayName
	}
	if m.ContextLength == nil {
		m.ContextLength = &info.ContextLength
	}
	if m.MaxOutputTokens == nil {
		m.MaxOutputTokens = &info.MaxOutputTokens
	}
	if info.Vision {
		hasVision := false
		for _, c := range m.Capabilities {
			if c == "vision" {
				hasVision = true
				break
			}
		}
		if !hasVision {
			m.Capabilities = append(m.Capabilities, "vision")
		}
	}
}

// fetchUpstreamModels fetches and returns the raw upstream model list.
func (b *OpenAIBackend) fetchUpstreamModels(ctx context.Context) ([]upstreamModel, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/models", nil)
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
		return nil, nil
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
