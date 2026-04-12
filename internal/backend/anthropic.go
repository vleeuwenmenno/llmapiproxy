package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/menno/llmapiproxy/internal/config"
)

const (
	anthropicDefaultVersion = "2023-06-01"
	anthropicHTTPTimeout    = 5 * time.Minute
)

// AnthropicBackend is a generic backend for Anthropic-compatible APIs that
// expose /v1/messages and /v1/models.
type AnthropicBackend struct {
	name         string
	baseURL      string
	apiKey       string
	extraHeaders map[string]string
	models       []config.ModelConfig
	client       *http.Client

	modelCacheTTL time.Duration
	cacheMu       sync.RWMutex
	cachedModels  []Model
	cacheExpiry   time.Time
}

func NewAnthropic(cfg config.BackendConfig, cacheTTL time.Duration) *AnthropicBackend {
	return &AnthropicBackend{
		name:         cfg.Name,
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		apiKey:       cfg.APIKey,
		extraHeaders: cfg.ExtraHeaders,
		models:       cfg.Models,
		client: &http.Client{
			Timeout: anthropicHTTPTimeout,
		},
		modelCacheTTL: cacheTTL,
	}
}

func (b *AnthropicBackend) Name() string { return b.name }

func (b *AnthropicBackend) SupportsModel(modelID string) bool {
	if len(b.models) == 0 {
		models := b.getCachedOrFetchModels()
		if len(models) == 0 {
			return true
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

func (b *AnthropicBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	body, err := b.rewriteBody(req)
	if err != nil {
		return nil, fmt.Errorf("rewriting request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.URL, err = b.endpointURL("/messages")
	if err != nil {
		return nil, fmt.Errorf("building messages URL: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var anthropicResp anthropicMessageResponse
	if err := json.Unmarshal(rawBody, &anthropicResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	chatResp := anthropicResponseToChatCompletion(&anthropicResp)
	if anthropicChatResponseEmpty(chatResp) && bytes.Contains(rawBody, []byte(`"choices"`)) {
		var directResp ChatCompletionResponse
		if err := json.Unmarshal(rawBody, &directResp); err == nil && len(directResp.Choices) > 0 {
			directResp.RawBody = rawBody
			return &directResp, nil
		}
	}
	chatResp.RawBody = rawBody
	return chatResp, nil
}

func (b *AnthropicBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	body, err := b.rewriteBody(req)
	if err != nil {
		return nil, fmt.Errorf("rewriting request: %w", err)
	}

	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	httpReq.URL, err = b.endpointURL("/messages")
	if err != nil {
		return nil, fmt.Errorf("building messages URL: %w", err)
	}
	b.setHeaders(httpReq, req.APIKeyOverride)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	return newAnthropicStreamReader(resp.Body), nil
}

type anthropicMessageRequest struct {
	Model         string                    `json:"model"`
	Messages      []anthropicRequestMessage `json:"messages"`
	System        string                    `json:"system,omitempty"`
	MaxTokens     int                       `json:"max_tokens"`
	Stream        bool                      `json:"stream,omitempty"`
	Temperature   *float64                  `json:"temperature,omitempty"`
	TopP          *float64                  `json:"top_p,omitempty"`
	StopSequences []string                  `json:"stop_sequences,omitempty"`
}

type anthropicRequestMessage struct {
	Role    string                         `json:"role"`
	Content []anthropicRequestContentBlock `json:"content"`
}

type anthropicRequestContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func (b *AnthropicBackend) rewriteBody(req *ChatCompletionRequest) ([]byte, error) {
	maxTokens := 1024
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		maxTokens = *req.MaxTokens
	} else if info := LookupKnownModel(req.Model); info != nil && info.MaxOutputTokens > 0 {
		if info.MaxOutputTokens < int64(maxTokens) {
			maxTokens = int(info.MaxOutputTokens)
		}
	}

	var raw map[string]json.RawMessage
	if len(req.RawBody) > 0 {
		_ = json.Unmarshal(req.RawBody, &raw)
	}

	systemParts := make([]string, 0)
	messages := make([]anthropicRequestMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		text := chatContentToText(msg.Content)
		switch msg.Role {
		case "system", "developer":
			if strings.TrimSpace(text) != "" {
				systemParts = append(systemParts, text)
			}
		default:
			// Some Anthropic-compatible providers reject {"type":"text"} blocks
			// with no text field. Skip empty text-only messages entirely.
			if strings.TrimSpace(text) == "" {
				continue
			}
			messages = append(messages, anthropicRequestMessage{
				Role: msg.Role,
				Content: []anthropicRequestContentBlock{
					{Type: "text", Text: text},
				},
			})
		}
	}

	out := anthropicMessageRequest{
		Model:       req.Model,
		Messages:    messages,
		System:      strings.Join(systemParts, "\n\n"),
		MaxTokens:   maxTokens,
		Stream:      req.Stream,
		Temperature: req.Temperature,
	}
	if raw != nil {
		if topPRaw, ok := raw["top_p"]; ok {
			var topP float64
			if err := json.Unmarshal(topPRaw, &topP); err == nil {
				out.TopP = &topP
			}
		}
		if stopRaw, ok := raw["stop"]; ok {
			var single string
			if err := json.Unmarshal(stopRaw, &single); err == nil && single != "" {
				out.StopSequences = []string{single}
			} else {
				var multi []string
				if err := json.Unmarshal(stopRaw, &multi); err == nil && len(multi) > 0 {
					out.StopSequences = multi
				}
			}
		}
	}

	return json.Marshal(out)
}

func chatContentToText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}

	textParts := make([]string, 0, len(parts))
	for _, part := range parts {
		if text, ok := part["text"].(string); ok && text != "" {
			textParts = append(textParts, text)
		}
	}
	return strings.Join(textParts, "\n")
}

type anthropicMessageResponse struct {
	ID           string                   `json:"id"`
	Type         string                   `json:"type"`
	Role         string                   `json:"role"`
	Model        string                   `json:"model"`
	Content      []anthropicResponseBlock `json:"content"`
	StopReason   string                   `json:"stop_reason"`
	StopSequence *string                  `json:"stop_sequence"`
	Usage        *anthropicUsage          `json:"usage,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponseBlock struct {
	Type    string          `json:"type"`
	Text    json.RawMessage `json:"text,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
	Value   json.RawMessage `json:"value,omitempty"`
}

func anthropicResponseToChatCompletion(resp *anthropicMessageResponse) *ChatCompletionResponse {
	stopReason := anthropicFinishReason(resp.StopReason)
	content := ""
	for _, block := range resp.Content {
		if text := anthropicResponseBlockText(block); text != "" {
			content += text
		}
	}
	contentBytes, _ := json.Marshal(content)

	out := &ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []Choice{
			{
				Index: 0,
				Message: &Message{
					Role:    "assistant",
					Content: contentBytes,
				},
				FinishReason: &stopReason,
			},
		},
	}
	if resp.Usage != nil {
		out.Usage = &Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		}
	}
	return out
}

func anthropicChatResponseEmpty(resp *ChatCompletionResponse) bool {
	if resp == nil || len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		return true
	}
	return strings.TrimSpace(chatContentToText(resp.Choices[0].Message.Content)) == ""
}

func anthropicResponseBlockText(block anthropicResponseBlock) string {
	if block.Type != "" && block.Type != "text" && block.Type != "output_text" {
		return ""
	}
	if text := anthropicLooseTextValue(block.Text); text != "" {
		return text
	}
	if text := anthropicLooseTextValue(block.Content); text != "" {
		return text
	}
	return anthropicLooseTextValue(block.Value)
}

func anthropicLooseTextValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"text", "value", "content"} {
			if nested, ok := obj[key]; ok {
				if text := anthropicLooseTextValue(nested); text != "" {
					return text
				}
			}
		}
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		parts := make([]string, 0, len(arr))
		for _, item := range arr {
			if text := anthropicLooseTextValue(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}

	return ""
}

func anthropicFinishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

type anthropicStreamReader struct {
	source      io.ReadCloser
	scanner     *bufio.Scanner
	responseID  string
	modelName   string
	buf         bytes.Buffer
	done        bool
	finishSent  bool
	currentType string
}

func newAnthropicStreamReader(source io.ReadCloser) *anthropicStreamReader {
	s := &anthropicStreamReader{
		source:     source,
		responseID: uuid.New().String(),
	}
	s.scanner = bufio.NewScanner(source)
	s.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return s
}

func (r *anthropicStreamReader) Read(p []byte) (int, error) {
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	if r.done {
		return 0, io.EOF
	}

	for r.buf.Len() == 0 && !r.done {
		if !r.scanner.Scan() {
			if !r.finishSent {
				r.writeFinishChunk("stop", nil)
			}
			r.buf.WriteString("data: [DONE]\n\n")
			r.done = true
			break
		}

		line := r.scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			r.currentType = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
		case strings.HasPrefix(line, "data: "):
			r.handleEventData(strings.TrimPrefix(line, "data: "))
		}
	}

	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	return 0, io.EOF
}

func (r *anthropicStreamReader) Close() error { return r.source.Close() }

func (r *anthropicStreamReader) handleEventData(data string) {
	switch r.currentType {
	case "message_start":
		var event struct {
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(data), &event); err == nil {
			if event.Message.ID != "" {
				r.responseID = event.Message.ID
			}
			r.modelName = event.Message.Model
		}
	case "content_block_delta":
		var event struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &event); err == nil && event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			r.writeTextChunk(event.Delta.Text)
		}
	case "message_delta":
		var event struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage *anthropicUsage `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(data), &event); err == nil && event.Delta.StopReason != "" {
			usage := (*Usage)(nil)
			if event.Usage != nil {
				usage = &Usage{
					PromptTokens:     event.Usage.InputTokens,
					CompletionTokens: event.Usage.OutputTokens,
					TotalTokens:      event.Usage.InputTokens + event.Usage.OutputTokens,
				}
			}
			r.writeFinishChunk(anthropicFinishReason(event.Delta.StopReason), usage)
		}
	case "message_stop":
		if !r.finishSent {
			r.writeFinishChunk("stop", nil)
		}
		r.buf.WriteString("data: [DONE]\n\n")
		r.done = true
	}
}

func (r *anthropicStreamReader) writeTextChunk(text string) {
	chunk := map[string]any{
		"id":      r.responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.modelName,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": text,
				},
				"finish_reason": nil,
			},
		},
	}
	b, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(b)
	r.buf.WriteString("\n\n")
}

func (r *anthropicStreamReader) writeFinishChunk(reason string, usage *Usage) {
	r.finishSent = true
	chunk := map[string]any{
		"id":      r.responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r.modelName,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": reason,
			},
		},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	b, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(b)
	r.buf.WriteString("\n\n")
}

func (b *AnthropicBackend) getCachedOrFetchModels() []Model {
	b.cacheMu.RLock()
	if b.cachedModels != nil {
		models := b.cachedModels
		b.cacheMu.RUnlock()
		return models
	}
	b.cacheMu.RUnlock()

	models, err := b.buildModelList(context.Background())
	if err != nil {
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

func (b *AnthropicBackend) ListModels(ctx context.Context) ([]Model, error) {
	if len(b.models) > 0 {
		return b.staticModelList(), nil
	}

	if b.modelCacheTTL > 0 {
		b.cacheMu.RLock()
		if !b.cacheExpiry.IsZero() && time.Now().Before(b.cacheExpiry) {
			cached := b.cachedModels
			b.cacheMu.RUnlock()
			return cached, nil
		}
		b.cacheMu.RUnlock()
	}

	models, err := b.buildModelList(ctx)
	if err != nil {
		if b.modelCacheTTL > 0 {
			b.cacheMu.RLock()
			if b.cachedModels != nil {
				cached := b.cachedModels
				b.cacheMu.RUnlock()
				return cached, nil
			}
			b.cacheMu.RUnlock()
		}
		return nil, err
	}

	if b.modelCacheTTL > 0 {
		b.cacheMu.Lock()
		b.cachedModels = models
		b.cacheExpiry = time.Now().Add(b.modelCacheTTL)
		b.cacheMu.Unlock()
	}
	return models, nil
}

func (b *AnthropicBackend) staticModelList() []Model {
	out := make([]Model, 0, len(b.models))
	for _, mc := range b.models {
		m := Model{
			ID:      mc.ID,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: b.name,
		}
		if mc.ContextLength != nil {
			m.ContextLength = mc.ContextLength
		}
		if mc.MaxOutputTokens != nil {
			m.MaxOutputTokens = mc.MaxOutputTokens
		}
		b.applyKnownDefaults(&m, mc.ID)
		out = append(out, m)
	}
	return out
}

func (b *AnthropicBackend) buildModelList(ctx context.Context) ([]Model, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("creating models request: %w", err)
	}
	httpReq.URL, err = b.endpointURL("/models")
	if err != nil {
		return nil, fmt.Errorf("building models URL: %w", err)
	}
	b.setHeaders(httpReq, "")

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic models returned status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data []struct {
			ID          string `json:"id"`
			Type        string `json:"type"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding models response: %w", err)
	}

	models := make([]Model, 0, len(result.Data))
	for _, item := range result.Data {
		created := time.Now().Unix()
		if item.CreatedAt != "" {
			if ts, err := time.Parse(time.RFC3339, item.CreatedAt); err == nil {
				created = ts.Unix()
			}
		}
		m := Model{
			ID:          item.ID,
			Object:      "model",
			Created:     created,
			OwnedBy:     b.name,
			DisplayName: item.DisplayName,
		}
		b.applyKnownDefaults(&m, item.ID)
		models = append(models, m)
	}
	return models, nil
}

func (b *AnthropicBackend) applyKnownDefaults(m *Model, modelID string) {
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
		m.Capabilities = append(m.Capabilities, "vision")
	}
}

func (b *AnthropicBackend) ClearModelCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cachedModels = nil
	b.cacheExpiry = time.Time{}
}

func (b *AnthropicBackend) endpointURL(path string) (*url.URL, error) {
	base := strings.TrimRight(b.baseURL, "/")
	if strings.HasSuffix(base, "/v1") {
		return url.Parse(base + path)
	}
	return url.Parse(base + "/v1" + path)
}

func (b *AnthropicBackend) setHeaders(httpReq *http.Request, apiKeyOverride string) {
	apiKey := b.apiKey
	if apiKeyOverride != "" {
		apiKey = apiKeyOverride
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	httpReq.Header.Set("x-api-key", apiKey)
	if httpReq.Header.Get("anthropic-version") == "" {
		httpReq.Header.Set("anthropic-version", anthropicDefaultVersion)
	}
	for k, v := range b.extraHeaders {
		httpReq.Header.Set(k, v)
	}
}
