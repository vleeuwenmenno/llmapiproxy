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
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/rs/zerolog/log"
)

const (
	// codexDefaultBaseURL is the default endpoint for Codex Responses API.
	codexDefaultBaseURL = "https://chatgpt.com/backend-api/codex"

	// codexResponsesPath is the path appended to the base URL for the responses endpoint.
	codexResponsesPath = "/responses"

	// codexMaxAuthRetries is the maximum number of re-authentication attempts on 401.
	codexMaxAuthRetries = 1

	// codexHTTPTimeout is the timeout for non-streaming Codex requests.
	codexHTTPTimeout = 5 * time.Minute
)



// CodexBackend implements the Backend interface for OpenAI Codex.
// It translates between the OpenAI ChatCompletion format and the Codex
// Responses API format, sending requests to chatgpt.com/backend-api/codex/responses.
//
// Authentication is via OAuth tokens managed by CodexOAuthHandler.
// Upstream 401 responses trigger token refresh with a single retry.
type CodexBackend struct {
	name              string
	baseURL           string
	models            []string
	client            *http.Client
	oauthHandler      *oauth.CodexOAuthHandler
	deviceCodeHandler *oauth.CodexDeviceCodeHandler
	tokenStore        *oauth.TokenStore
	cfg               config.BackendConfig

	// Model list cache.
	modelCacheTTL time.Duration
	cacheMu       sync.RWMutex
	cachedModels  []Model
	cacheExpiry   time.Time
}

// NewCodexBackend creates a new CodexBackend from the given configuration.
// The deviceCodeHandler is optional and enables device code flow as an alternative
// login method for headless/SSH environments.
// cacheTTL controls how long the upstream model list is cached; 0 means no caching.
func NewCodexBackend(cfg config.BackendConfig, oauthHandler *oauth.CodexOAuthHandler, tokenStore *oauth.TokenStore, deviceCodeHandler *oauth.CodexDeviceCodeHandler, cacheTTL time.Duration) *CodexBackend {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	return &CodexBackend{
		name:              cfg.Name,
		baseURL:           baseURL,
		models:            cfg.ModelIDs(),
		client:            &http.Client{Timeout: codexHTTPTimeout},
		oauthHandler:      oauthHandler,
		deviceCodeHandler: deviceCodeHandler,
		tokenStore:        tokenStore,
		cfg:               cfg,
		modelCacheTTL:     cacheTTL,
	}
}

// Name returns the backend's configured name (used as model prefix).
func (b *CodexBackend) Name() string { return b.name }

// SupportsModel returns true if this backend can handle the given model ID.
// If a static model list is configured, only those are accepted.
// Otherwise, the cached model list (populated by ListModels) is consulted.
// If the cache is empty (not yet warmed), false is returned.
func (b *CodexBackend) SupportsModel(modelID string) bool {
	if len(b.models) > 0 {
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
	// Check the cached model list.
	b.cacheMu.RLock()
	cached := b.cachedModels
	b.cacheMu.RUnlock()
	for _, m := range cached {
		if m.ID == modelID {
			return true
		}
	}
	return false
}

// ClearModelCache clears the cached model list, forcing a fresh fetch on the
// next ListModels call.
func (b *CodexBackend) ClearModelCache() {
	b.cacheMu.Lock()
	defer b.cacheMu.Unlock()
	b.cachedModels = nil
	b.cacheExpiry = time.Time{}
}

// --- ChatCompletion ↔ Responses API format translation ---

// codexRequest is the Responses API request format.
type codexRequest struct {
	Model        string          `json:"model"`
	Input        json.RawMessage `json:"input"`
	Stream       bool            `json:"stream,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	Instructions string          `json:"instructions"`
	Store        bool            `json:"store"`

	// Preserve extra fields from the original request body.
	Extra map[string]json.RawMessage `json:"-"`
}

// MarshalJSON implements custom marshaling to include extra fields.
func (r codexRequest) MarshalJSON() ([]byte, error) {
	m := make(map[string]json.RawMessage)

	// Marshal known fields.
	if r.Model != "" {
		b, _ := json.Marshal(r.Model)
		m["model"] = b
	}
	if r.Input != nil {
		m["input"] = r.Input
	}
	if r.Stream {
		m["stream"] = json.RawMessage(`true`)
	}
	if r.Temperature != nil {
		b, _ := json.Marshal(*r.Temperature)
		m["temperature"] = b
	}
	if r.TopP != nil {
		b, _ := json.Marshal(*r.TopP)
		m["top_p"] = b
	}
	if r.Instructions != "" {
		b, _ := json.Marshal(r.Instructions)
		m["instructions"] = b
	}

	// Always send store=false (Codex API rejects requests where store defaults to true).
	m["store"] = json.RawMessage(`false`)

	// Add extra fields (not overwriting known fields).
	for k, v := range r.Extra {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}

	return json.Marshal(m)
}

// codexInputMessage represents a message in the Responses API input format.
type codexInputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Type    string `json:"type,omitempty"`
}

// codexResponse is the Responses API response format.
type codexResponse struct {
	ID        string            `json:"id"`
	Object    string            `json:"object"`
	CreatedAt int64             `json:"created_at"`
	Status    string            `json:"status"`
	Model     string            `json:"model"`
	Output    []codexOutputItem `json:"output"`
	Usage     *codexUsage       `json:"usage,omitempty"`
	Error     *codexError       `json:"error,omitempty"`
}

// codexOutputItem is an item in the Responses API output array.
type codexOutputItem struct {
	Type    string               `json:"type"`
	ID      string               `json:"id"`
	Role    string               `json:"role"`
	Status  string               `json:"status"`
	Content []codexOutputContent `json:"content"`
}

// codexOutputContent is a content part in the output.
type codexOutputContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// codexUsage maps the Responses API usage object.
type codexUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// codexError represents an error in the Responses API response.
type codexError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// translateToCodexRequest converts a ChatCompletionRequest to a Codex Responses API request.
func translateToCodexRequest(req *ChatCompletionRequest) (*codexRequest, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages array is empty; at least one message is required")
	}

	// Separate system messages from conversation messages.
	var instructions string
	var conversationMessages []codexInputMessage

	for _, msg := range req.Messages {
		if msg.Role == "system" || msg.Role == "developer" {
			// Use the last system/developer message as instructions.
			var contentStr string
			_ = json.Unmarshal(msg.Content, &contentStr)
			instructions = contentStr
			continue
		}
		conversationMessages = append(conversationMessages, codexInputMessage{
			Role:    msg.Role,
			Content: func() string { var s string; _ = json.Unmarshal(msg.Content, &s); return s }(),
			Type:    "message",
		})
	}

	// Build the input for the Responses API.
	// Always send as a list of messages — the Codex API requires a list.
	var input json.RawMessage
	input, _ = json.Marshal(conversationMessages)

	var raw map[string]json.RawMessage
	if len(req.RawBody) > 0 {
		_ = json.Unmarshal(req.RawBody, &raw)
	}

	supportsSampling := codexSupportsSampling(req.Model, raw)

	var temperature *float64
	if supportsSampling {
		temperature = req.Temperature
	}

	codexReq := &codexRequest{
		Model:        req.Model,
		Input:        input,
		Stream:       false, // set to true for streaming calls
		Temperature:  temperature,
		Instructions: instructions, // Codex API requires non-empty instructions
		Extra:        make(map[string]json.RawMessage),
	}

	// Codex API requires non-empty instructions — default to a minimal prompt.
	if codexReq.Instructions == "" {
		codexReq.Instructions = "You are a helpful assistant."
	}

	// Preserve extra fields from the raw body.
	if len(raw) > 0 {
		knownFields := map[string]bool{
			"model": true, "messages": true, "stream": true,
			"temperature": true, "max_tokens": true, "max_output_tokens": true,
			"store": true, "top_p": true, "frequency_penalty": true,
			"presence_penalty": true, "n": true, "stop": true,
			"user": true, "logprobs": true, "top_logprobs": true,
			"response_format": true, "seed": true, "tools": true,
			"tool_choice": true, "parallel_tool_calls": true,
		}
		for k, v := range raw {
			if !knownFields[k] {
				codexReq.Extra[k] = v
			}
		}

		// Extract top_p only for models that currently support sampling params.
		if supportsSampling {
			if topPRaw, ok := raw["top_p"]; ok {
				var topP float64
				if err := json.Unmarshal(topPRaw, &topP); err == nil {
					codexReq.TopP = &topP
				}
			}
		}
	}

	return codexReq, nil
}

func codexSupportsSampling(modelID string, raw map[string]json.RawMessage) bool {
	modelID = strings.ToLower(strings.TrimSpace(modelID))

	// GPT-5.1 is the one GPT-5 family exception that can support sampling when
	// the caller explicitly disables reasoning.
	if strings.HasPrefix(modelID, "gpt-5.1") {
		return codexReasoningEffort(raw) == "none"
	}

	// GPT-5, Codex, and o-series reasoning models reject temperature/top_p.
	if strings.HasPrefix(modelID, "gpt-5") ||
		strings.HasPrefix(modelID, "o3") ||
		strings.HasPrefix(modelID, "o4") ||
		strings.HasPrefix(modelID, "codex-mini") {
		return false
	}

	if info := LookupKnownModel(modelID); info != nil && info.UseMaxCompletionTokens {
		return false
	}

	return true
}

func codexReasoningEffort(raw map[string]json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	if effortRaw, ok := raw["reasoning_effort"]; ok {
		var effort string
		if err := json.Unmarshal(effortRaw, &effort); err == nil {
			return strings.ToLower(strings.TrimSpace(effort))
		}
	}

	if reasoningRaw, ok := raw["reasoning"]; ok {
		var reasoning struct {
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(reasoningRaw, &reasoning); err == nil {
			return strings.ToLower(strings.TrimSpace(reasoning.Effort))
		}
	}

	return ""
}

// translateFromCodexResponse converts a Codex Responses API response to a ChatCompletion response.
func translateFromCodexResponse(codexResp *codexResponse) (*ChatCompletionResponse, error) {
	chatResp := &ChatCompletionResponse{
		ID:      codexResp.ID,
		Object:  "chat.completion",
		Created: codexResp.CreatedAt,
		Model:   codexResp.Model,
	}

	// Check for error in the response.
	if codexResp.Error != nil {
		return nil, &BackendError{
			StatusCode: http.StatusInternalServerError,
			Body:       codexResp.Error.Message,
			Err:        fmt.Errorf("codex error: %s: %s", codexResp.Error.Code, codexResp.Error.Message),
		}
	}

	// Check for incomplete status.
	if codexResp.Status == "incomplete" {
		// This might indicate max_output_tokens was reached.
		// We'll still try to extract the output.
	}

	// Extract the assistant message from the output.
	for _, item := range codexResp.Output {
		if item.Type == "message" && item.Role == "assistant" {
			var contentParts []string
			for _, c := range item.Content {
				if c.Type == "output_text" {
					contentParts = append(contentParts, c.Text)
				}
			}
			content := strings.Join(contentParts, "")

			finishReason := "stop"
			if codexResp.Status == "incomplete" {
				finishReason = "length"
			}

			contentBytes, _ := json.Marshal(content)
			chatResp.Choices = append(chatResp.Choices, Choice{
				Index: 0,
				Message: &Message{
					Role:    "assistant",
					Content: contentBytes,
				},
				FinishReason: &finishReason,
			})
		}
	}

	// If no message was found, return an empty choice with stop.
	if len(chatResp.Choices) == 0 {
		stop := "stop"
		chatResp.Choices = append(chatResp.Choices, Choice{
			Index: 0,
			Message: &Message{
				Role:    "assistant",
				Content: json.RawMessage(`""`),
			},
			FinishReason: &stop,
		})
	}

	// Map usage stats.
	if codexResp.Usage != nil {
		chatResp.Usage = &Usage{
			PromptTokens:     codexResp.Usage.InputTokens,
			CompletionTokens: codexResp.Usage.OutputTokens,
			TotalTokens:      codexResp.Usage.TotalTokens,
		}
	}

	return chatResp, nil
}

// readCodexSSEToCompletion reads the full Codex SSE stream, collecting text delta
// events and extracting the response.completed event to build a codexResponse.
// This is used when we force streaming internally for prompt caching consistency.
func readCodexSSEToCompletion(reader io.Reader) (*codexResponse, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var textParts []string
	var completedResp *codexResponse

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:]
		if data == "[DONE]" {
			break
		}

		// Parse the event type.
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.output_text.delta":
			var delta struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &delta); err == nil && delta.Delta != "" {
				textParts = append(textParts, delta.Delta)
			}

		case "response.completed":
			var completed struct {
				Response *codexResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &completed); err == nil && completed.Response != nil {
				completedResp = completed.Response
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}

	// If we got a response.completed event, use it as the base response.
	if completedResp != nil {
		// If there's no output but we collected text, build the output.
		if len(completedResp.Output) == 0 && len(textParts) > 0 {
			completedResp.Output = []codexOutputItem{
				{
					Type:   "message",
					Role:   "assistant",
					Status: "completed",
					Content: []codexOutputContent{
						{
							Type: "output_text",
							Text: strings.Join(textParts, ""),
						},
					},
				},
			}
		}
		return completedResp, nil
	}

	// No response.completed event — synthesize from collected deltas.
	if len(textParts) == 0 {
		return nil, fmt.Errorf("no response data received from Codex SSE stream")
	}

	fullText := strings.Join(textParts, "")
	return &codexResponse{
		Object: "response",
		Status: "completed",
		Output: []codexOutputItem{
			{
				Type:   "message",
				Role:   "assistant",
				Status: "completed",
				Content: []codexOutputContent{
					{
						Type: "output_text",
						Text: fullText,
					},
				},
			},
		},
	}, nil
}

// --- Backend interface implementation ---

// ChatCompletion sends a non-streaming chat completion request via the Codex Responses API.
func (b *CodexBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return b.doChatCompletion(ctx, req, 0)
}

func (b *CodexBackend) doChatCompletion(ctx context.Context, req *ChatCompletionRequest, retryCount int) (*ChatCompletionResponse, error) {
	// Validate messages.
	if len(req.Messages) == 0 {
		return nil, &BackendError{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"messages array is empty; at least one message is required","type":"invalid_request_error"}}`,
			Err:        fmt.Errorf("codex backend %s: messages array is empty", b.name),
		}
	}

	// Get a valid access token.
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: %w", b.name, err)
	}

	// Translate to Codex request.
	codexReq, err := translateToCodexRequest(req)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: translating request: %w", b.name, err)
	}
	// Force streaming mode internally for prompt caching consistency.
	// Codex servers cache prompts more effectively when stream=true is used,
	// matching the CLIProxyAPIPlus pattern. The client still receives a
	// non-streaming response.
	codexReq.Stream = true

	body, err := json.Marshal(codexReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: marshaling request: %w", b.name, err)
	}

	endpoint := b.baseURL + codexResponsesPath
	// Use a client without timeout for streaming (same as ChatCompletionStream).
	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: sending request: %w", b.name, err)
	}
	defer resp.Body.Close()

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < codexMaxAuthRetries {
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("codex backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doChatCompletion(ctx, req, retryCount+1)
	}

	// Handle non-200 responses.
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("codex backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	// Read the full SSE stream and extract the response.completed event.
	// We collect all text deltas and the final completed event to build
	// a non-streaming ChatCompletionResponse.
	codexResp, err := readCodexSSEToCompletion(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: reading streaming response: %w", b.name, err)
	}

	// Translate back to ChatCompletion format.
	return translateFromCodexResponse(codexResp)
}

// ChatCompletionStream sends a streaming chat completion request via the Codex Responses API.
// The Codex streaming format uses SSE events like "response.output_text.delta".
// These are translated to OpenAI ChatCompletion streaming chunks.
func (b *CodexBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	return b.doChatCompletionStream(ctx, req, 0)
}

func (b *CodexBackend) doChatCompletionStream(ctx context.Context, req *ChatCompletionRequest, retryCount int) (io.ReadCloser, error) {
	// Validate messages.
	if len(req.Messages) == 0 {
		return nil, &BackendError{
			StatusCode: http.StatusBadRequest,
			Body:       `{"error":{"message":"messages array is empty; at least one message is required","type":"invalid_request_error"}}`,
			Err:        fmt.Errorf("codex backend %s: messages array is empty", b.name),
		}
	}

	// Get a valid access token.
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: %w", b.name, err)
	}

	// Translate to Codex request.
	codexReq, err := translateToCodexRequest(req)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: translating request: %w", b.name, err)
	}
	codexReq.Stream = true

	body, err := json.Marshal(codexReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: marshaling request: %w", b.name, err)
	}

	endpoint := b.baseURL + codexResponsesPath
	client := &http.Client{} // No timeout for streaming.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: sending request: %w", b.name, err)
	}

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < codexMaxAuthRetries {
		resp.Body.Close()
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("codex backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doChatCompletionStream(ctx, req, retryCount+1)
	}

	// Handle non-200 responses.
	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("codex backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	// Return a reader that translates Codex SSE events to ChatCompletion SSE chunks.
	return newCodexStreamReader(resp.Body, uuid.New().String(), b.name), nil
}

// ListModels returns the list of models this Codex backend supports.
// If a static model list is configured, it returns those (enriched from
// upstream when available). Otherwise it fetches from the upstream /models
// endpoint and falls back to defaultCodexModels on any error.
func (b *CodexBackend) ListModels(ctx context.Context) ([]Model, error) {
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

	// Slow path: build model list.
	log.Debug().Str("backend", b.name).Msg("building model list")
	models, err := b.buildCodexModelList(ctx)
	if err != nil {
		// Stale-while-error: return stale cache if available.
		if b.modelCacheTTL > 0 {
			b.cacheMu.RLock()
			if b.cachedModels != nil {
				cached := b.cachedModels
				b.cacheMu.RUnlock()
				log.Warn().Err(err).Str("backend", b.name).Int("models", len(cached)).Msg("model list build failed, returning stale cache")
				return cached, nil
			}
			b.cacheMu.RUnlock()
		}
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

// buildCodexModelList builds the model list from config, upstream, or defaults.
func (b *CodexBackend) buildCodexModelList(ctx context.Context) ([]Model, error) {
	if len(b.models) > 0 {
		// Static model list configured — try to enrich from upstream, ignore errors.
		upstreamMap := b.fetchUpstreamModelMap(ctx)
		seen := make(map[string]bool, len(b.models))
		models := make([]Model, 0, len(b.models))
		for _, id := range b.models {
			if seen[id] {
				continue
			}
			seen[id] = true
			if u, ok := upstreamMap[id]; ok {
				// Upstream data found — use it, enrich with known DB.
				m := u.toModel(b.name)
				applyKnownDefaults(&m, id)
				models = append(models, m)
			} else {
				models = append(models, codexModel(id, b.name))
			}
		}
		return models, nil
	}

	// Dynamic: fetch from upstream API.
	upstreamModels, err := b.fetchUpstreamModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching upstream models: %w", err)
	}
	if len(upstreamModels) == 0 {
		return nil, fmt.Errorf("upstream returned empty model list")
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

// codexModelsURL returns the URL for the models endpoint.
// The official Codex CLI appends /models to the base URL for both
// ChatGPT-style (https://chatgpt.com/backend-api/codex/models) and
// API-style (https://api.openai.com/v1/models) endpoints.
func (b *CodexBackend) codexModelsURL() string {
	return b.baseURL + "/models"
}

// fetchUpstreamModels fetches the model list from the Codex upstream models endpoint.
// Returns an error if the upstream is unreachable or authentication is not available.
func (b *CodexBackend) fetchUpstreamModels(ctx context.Context) ([]upstreamModel, error) {
	// Check if we have authentication available before attempting.
	if b.oauthHandler == nil {
		return nil, fmt.Errorf("no OAuth handler configured")
	}
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}

	modelsURL := b.codexModelsURL()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, token)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetching models from %s: %w", modelsURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upstream %s returned status %d: %s", modelsURL, resp.StatusCode, string(body))
	}

	var list upstreamModelList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("decoding models: %w", err)
	}
	return list.Data, nil
}

// FetchUpstreamModelsRaw returns the raw HTTP response body from the upstream models endpoint.
// This is useful for debugging — it shows exactly what the upstream API returns.
func (b *CodexBackend) FetchUpstreamModelsRaw(ctx context.Context) (*UpstreamModelsResponse, error) {
	if b.oauthHandler == nil {
		return &UpstreamModelsResponse{
			Backend:    b.name,
			URL:        b.codexModelsURL(),
			StatusCode: 0,
			Error:      "no OAuth handler configured",
		}, nil
	}
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return &UpstreamModelsResponse{
			Backend:    b.name,
			URL:        b.codexModelsURL(),
			StatusCode: 0,
			Error:      fmt.Sprintf("getting access token: %v", err),
		}, nil
	}

	modelsURL := b.codexModelsURL()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, token)

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

// fetchUpstreamModelMap returns a map of model ID → upstream model for enrichment.
// Errors are silently ignored — it is only used as best-effort enrichment.
func (b *CodexBackend) fetchUpstreamModelMap(ctx context.Context) map[string]upstreamModel {
	models, _ := b.fetchUpstreamModels(ctx)
	m := make(map[string]upstreamModel, len(models))
	for _, u := range models {
		m[u.ID] = u
	}
	return m
}

func codexModel(id string, owner string) Model {
	model := Model{
		ID:      id,
		Object:  "model",
		Created: time.Now().Unix(),
		OwnedBy: owner,
	}

	if info := LookupKnownModel(id); info != nil {
		model.DisplayName = info.DisplayName
		model.ContextLength = &info.ContextLength
		model.MaxOutputTokens = &info.MaxOutputTokens
		if info.Vision {
			model.Capabilities = append(model.Capabilities, "vision")
		}
	}

	return model
}

// --- Helper methods ---

// getAccessToken returns a valid Codex access token.
func (b *CodexBackend) getAccessToken(ctx context.Context) (string, error) {
	token := b.tokenStore.ValidToken()
	if token != nil {
		return token.AccessToken, nil
	}

	// Try to refresh the token.
	tokenData, err := b.oauthHandler.RefreshWithRetry(ctx)
	if err != nil {
		return "", fmt.Errorf("Codex authentication required; complete OAuth setup via the web UI: %w", err)
	}

	return tokenData.AccessToken, nil
}

// setHeaders sets all required headers on the HTTP request.
func (b *CodexBackend) setHeaders(httpReq *http.Request, accessToken string) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+accessToken)
	httpReq.Header.Set("User-Agent", "llmapiproxy/1.0")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Request-Id", uuid.New().String())
}

// --- Codex stream reader (translates Codex SSE → ChatCompletion SSE) ---

// codexStreamReader translates Codex Responses API SSE events into
// OpenAI ChatCompletion SSE chunks in real time.
type codexStreamReader struct {
	source     io.ReadCloser
	scanner    *bufio.Scanner
	responseID string
	modelName  string
	buf        bytes.Buffer
	done       bool
	mu         sync.Mutex

	// Accumulated usage from the response.completed event.
	usage *codexUsage
}

func newCodexStreamReader(source io.ReadCloser, responseID string, modelName string) *codexStreamReader {
	s := &codexStreamReader{
		source:     source,
		responseID: responseID,
		modelName:  modelName,
	}
	s.scanner = bufio.NewScanner(source)
	s.scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return s
}

// Read implements io.Reader. It reads translated SSE chunks.
func (r *codexStreamReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// If we have buffered data, return it.
	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}

	if r.done {
		return 0, io.EOF
	}

	// Process Codex SSE events to produce ChatCompletion SSE chunks.
	for r.buf.Len() == 0 && !r.done {
		if !r.scanner.Scan() {
			// Stream ended — write [DONE] sentinel.
			r.buf.WriteString("data: [DONE]\n\n")
			r.done = true
			break
		}

		line := r.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:]
		if data == "[DONE]" {
			r.buf.WriteString("data: [DONE]\n\n")
			r.done = true
			break
		}

		// Parse the Codex SSE event type.
		var event struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		switch event.Type {
		case "response.output_text.delta":
			r.handleTextDelta(data)
		case "response.output_text.done", "response.completed":
			r.handleCompleted(data)
		}
	}

	if r.buf.Len() > 0 {
		return r.buf.Read(p)
	}
	if r.done {
		return 0, io.EOF
	}
	return 0, nil
}

// handleTextDelta translates a Codex text delta event to a ChatCompletion chunk.
func (r *codexStreamReader) handleTextDelta(data string) {
	var event struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return
	}

	chunk := ChatCompletionStreamChunk{
		ID:      r.responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   r.modelName,
		Choices: []ChunkChoice{
			{
				Index: 0,
				Delta: &Message{
					Content: func() json.RawMessage { b, _ := json.Marshal(event.Delta); return b }(),
				},
				FinishReason: nil,
			},
		},
	}

	b, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(b)
	r.buf.WriteString("\n\n")
}

// handleCompleted writes the final chunk with finish_reason.
func (r *codexStreamReader) handleCompleted(data string) {
	finishReason := "stop"

	// Try to extract usage from response.completed.
	var completedEvent struct {
		Response *codexResponse `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &completedEvent); err == nil && completedEvent.Response != nil {
		if completedEvent.Response.Status == "incomplete" {
			finishReason = "length"
		}
		r.usage = completedEvent.Response.Usage
	}

	chunk := ChatCompletionStreamChunk{
		ID:      r.responseID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   r.modelName,
		Choices: []ChunkChoice{
			{
				Index:        0,
				Delta:        &Message{},
				FinishReason: &finishReason,
			},
		},
	}

	b, _ := json.Marshal(chunk)
	r.buf.WriteString("data: ")
	r.buf.Write(b)
	r.buf.WriteString("\n\n")
}

// Close closes the underlying stream.
func (r *codexStreamReader) Close() error {
	return r.source.Close()
}

// ChatCompletionStreamChunk is a streaming chunk in the OpenAI format.
type ChatCompletionStreamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
}

// ChunkChoice represents a choice in a streaming chunk.
type ChunkChoice struct {
	Index        int      `json:"index"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason,omitempty"`
}

// --- ResponsesBackend interface implementation (native passthrough) ---

// Responses sends a non-streaming Responses API request natively (no translation).
func (b *CodexBackend) Responses(ctx context.Context, req *ResponsesRequest) (*ResponsesResponse, error) {
	return b.doResponses(ctx, req, 0)
}

func (b *CodexBackend) doResponses(ctx context.Context, req *ResponsesRequest, retryCount int) (*ResponsesResponse, error) {
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: %w", b.name, err)
	}

	// Use the raw body directly — no format translation.
	body := req.RawBody

	endpoint := b.baseURL + codexResponsesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: sending request: %w", b.name, err)
	}
	defer resp.Body.Close()

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < codexMaxAuthRetries {
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("codex backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doResponses(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("codex backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: reading response: %w", b.name, err)
	}

	return &ResponsesResponse{Body: respBody}, nil
}

// ResponsesStream sends a streaming Responses API request natively (no translation).
// Returns the raw SSE stream from the upstream — no format translation.
func (b *CodexBackend) ResponsesStream(ctx context.Context, req *ResponsesRequest) (io.ReadCloser, error) {
	return b.doResponsesStream(ctx, req, 0)
}

func (b *CodexBackend) doResponsesStream(ctx context.Context, req *ResponsesRequest, retryCount int) (io.ReadCloser, error) {
	token, err := b.getAccessToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: %w", b.name, err)
	}

	// Use the raw body directly — no format translation.
	body := req.RawBody

	endpoint := b.baseURL + codexResponsesPath
	client := &http.Client{} // No timeout for streaming.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: creating request: %w", b.name, err)
	}
	b.setHeaders(httpReq, token)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("codex backend %s: sending request: %w", b.name, err)
	}

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < codexMaxAuthRetries {
		resp.Body.Close()
		if _, refreshErr := b.oauthHandler.RefreshToken(ctx); refreshErr != nil {
			return nil, fmt.Errorf("codex backend %s: token refresh failed on 401: %w", b.name, refreshErr)
		}
		return b.doResponsesStream(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("codex backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	// Return the raw stream — no translation to chat completion format.
	return resp.Body, nil
}

// --- OAuthStatusProvider interface ---

// OAuthStatus returns the current authentication status of the Codex backend.
func (b *CodexBackend) OAuthStatus() OAuthStatus {
	status := OAuthStatus{
		BackendName: b.name,
		BackendType: "codex",
		TokenState:  "missing",
	}

	token := b.tokenStore.Get()
	if token != nil {
		status.Authenticated = !token.IsExpired()
		status.TokenSource = token.Source
		status.ExpiresAt = token.ExpiresAt
		status.ObtainedAt = token.ObtainedAt
		if !token.ExpiresAt.IsZero() {
			status.TokenExpiry = token.ExpiresAt.Format(time.RFC3339)
		}
		if !token.ObtainedAt.IsZero() {
			status.LastRefresh = token.ObtainedAt.Format(time.RFC3339)
		}
		status.NeedsReauth = token.IsExpired() && token.RefreshToken == ""
		// Compute visual indicator state.
		if token.IsExpired() {
			status.TokenState = "expired"
		} else if time.Until(token.ExpiresAt) < 5*time.Minute {
			status.TokenState = "expiring"
		} else {
			status.TokenState = "valid"
		}
	} else {
		// No token stored at all — not "needs re-auth", just not connected.
		// NeedsReauth is only true when a token exists but is expired and can't be refreshed.
	}

	return status
}

// --- OAuthLoginHandler interface ---

// InitiateLogin starts the OAuth PKCE flow for Codex authentication.
// Returns the authorization URL to redirect the user to and a state parameter.
func (b *CodexBackend) InitiateLogin() (authURL string, state string, err error) {
	return b.oauthHandler.AuthorizeURL()
}

// --- OAuthCallbackHandler interface ---

// HandleCallback processes the OAuth callback by exchanging the authorization
// code for tokens using the stored PKCE verifier.
func (b *CodexBackend) HandleCallback(ctx context.Context, code string, state string) error {
	_, err := b.oauthHandler.HandleCallback(ctx, code, state)
	return err
}

// --- OAuthDisconnectHandler interface ---

// Disconnect clears all stored tokens for the Codex backend.
func (b *CodexBackend) Disconnect() error {
	return b.tokenStore.Clear()
}

// RefreshOAuthStatus proactively re-validates or refreshes the Codex token.
// It reuses the normal coordinated refresh path so the web UI can check the
// status of this backend without affecting other OAuth backends.
func (b *CodexBackend) RefreshOAuthStatus(ctx context.Context) error {
	if b.oauthHandler == nil {
		return fmt.Errorf("codex backend %s: oauth handler not configured", b.name)
	}
	_, err := b.oauthHandler.RefreshWithRetry(ctx)
	if err != nil {
		return fmt.Errorf("codex backend %s: token refresh failed: %w", b.name, err)
	}
	return nil
}

// GetOAuthHandler returns the underlying CodexOAuthHandler (for testing).
func (b *CodexBackend) GetOAuthHandler() *oauth.CodexOAuthHandler {
	return b.oauthHandler
}

// GetTokenStore returns the underlying TokenStore (for status checking).
func (b *CodexBackend) GetTokenStore() *oauth.TokenStore {
	return b.tokenStore
}

// --- OAuthDeviceCodeLoginHandler interface ---

// InitiateDeviceCodeLogin starts the device code flow for Codex authentication.
// This is an alternative to the browser-based PKCE flow, designed for headless/SSH
// environments where opening a browser is impractical.
//
// Returns JSON-encoded DeviceCodeLoginInfo containing the user_code and verification_uri
// that the UI should display to the user, plus a state (device_code) for tracking.
// The polling happens in the background — the tokens are stored automatically.
func (b *CodexBackend) InitiateDeviceCodeLogin() (authURL string, state string, err error) {
	if b.deviceCodeHandler == nil {
		return "", "", fmt.Errorf("codex backend %s: device code flow not available", b.name)
	}

	resp, err := b.deviceCodeHandler.InitiateDeviceCode(context.Background())
	if err != nil {
		return "", "", fmt.Errorf("codex device code flow: %w", err)
	}

	// Return JSON-encoded device code info as the "auth URL" (the web handler
	// will parse this and display the device code page).
	info := DeviceCodeLoginInfo{
		DeviceCode:      resp.DeviceCode,
		UserCode:        resp.UserCode,
		VerificationURI: resp.VerificationURI,
		ExpiresIn:       resp.ExpiresIn,
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return "", "", fmt.Errorf("encoding device code info: %w", err)
	}

	// Use the device_code as the state.
	state = resp.DeviceCode
	authURL = string(infoJSON)

	log.Info().Str("backend", b.name).Str("user_code", resp.UserCode).Msg("device code flow initiated")

	// Start polling in the background — the result will be stored automatically.
	go func() {
		bgCtx := context.Background()
		_, pollErr := b.deviceCodeHandler.WaitForAuthorization(bgCtx, resp)
		if pollErr != nil {
			log.Warn().Err(pollErr).Str("backend", b.name).Msg("device code authorization failed")
		} else {
			log.Info().Str("backend", b.name).Msg("device code authorization completed successfully")
		}
	}()

	return authURL, state, nil
}

// SupportsDeviceCodeFlow returns true if this backend has a device code handler configured.
func (b *CodexBackend) SupportsDeviceCodeFlow() bool {
	return b.deviceCodeHandler != nil
}
