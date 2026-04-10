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
	models       []string
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
		if m == modelID {
			return true
		}
		if strings.HasSuffix(m, "/*") {
			prefix := strings.TrimSuffix(m, "/*")
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
	b.setHeaders(httpReq)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))
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
	b.setHeaders(httpReq)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody))
	}

	return resp.Body, nil
}

func (b *OpenAIBackend) ListModels(ctx context.Context) ([]Model, error) {
	if len(b.models) > 0 {
		models := make([]Model, 0, len(b.models))
		for _, m := range b.models {
			models = append(models, Model{
				ID:      m,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: b.name,
			})
		}
		return models, nil
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var list ModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding models: %w", err)
	}

	for i := range list.Data {
		list.Data[i].OwnedBy = b.name
	}
	return list.Data, nil
}

func (b *OpenAIBackend) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	for k, v := range b.extraHeaders {
		req.Header.Set(k, v)
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
