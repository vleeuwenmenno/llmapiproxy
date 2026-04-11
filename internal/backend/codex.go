package backend

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
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
}

// NewCodexBackend creates a new CodexBackend from the given configuration.
// The deviceCodeHandler is optional and enables device code flow as an alternative
// login method for headless/SSH environments.
func NewCodexBackend(cfg config.BackendConfig, oauthHandler *oauth.CodexOAuthHandler, tokenStore *oauth.TokenStore, deviceCodeHandler *oauth.CodexDeviceCodeHandler) *CodexBackend {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = codexDefaultBaseURL
	}

	return &CodexBackend{
		name:              cfg.Name,
		baseURL:           baseURL,
		models:            cfg.Models,
		client:            &http.Client{Timeout: codexHTTPTimeout},
		oauthHandler:      oauthHandler,
		deviceCodeHandler: deviceCodeHandler,
		tokenStore:        tokenStore,
		cfg:               cfg,
	}
}

// Name returns the backend's configured name (used as model prefix).
func (b *CodexBackend) Name() string { return b.name }

// SupportsModel returns true if this backend can handle the given model ID.
// If no models are configured, all models are accepted.
func (b *CodexBackend) SupportsModel(modelID string) bool {
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

// --- ChatCompletion ↔ Responses API format translation ---

// codexRequest is the Responses API request format.
type codexRequest struct {
	Model         string          `json:"model"`
	Input         json.RawMessage `json:"input"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	MaxOutputTokens *int          `json:"max_output_tokens,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Instructions  string          `json:"instructions,omitempty"`

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
	if r.MaxOutputTokens != nil {
		b, _ := json.Marshal(*r.MaxOutputTokens)
		m["max_output_tokens"] = b
	}
	if r.TopP != nil {
		b, _ := json.Marshal(*r.TopP)
		m["top_p"] = b
	}
	if r.Instructions != "" {
		b, _ := json.Marshal(r.Instructions)
		m["instructions"] = b
	}

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
	ID         string `json:"id"`
	Object     string `json:"object"`
	CreatedAt  int64  `json:"created_at"`
	Status     string `json:"status"`
	Model      string `json:"model"`
	Output     []codexOutputItem `json:"output"`
	Usage      *codexUsage       `json:"usage,omitempty"`
	Error      *codexError       `json:"error,omitempty"`
}

// codexOutputItem is an item in the Responses API output array.
type codexOutputItem struct {
	Type    string              `json:"type"`
	ID      string              `json:"id"`
	Role    string              `json:"role"`
	Status  string              `json:"status"`
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
			instructions = msg.Content
			continue
		}
		conversationMessages = append(conversationMessages, codexInputMessage{
			Role:    msg.Role,
			Content: msg.Content,
			Type:    "message",
		})
	}

	// Build the input for the Responses API.
	var input json.RawMessage
	if len(conversationMessages) == 1 {
		// Single user message — can be sent as a plain string.
		input, _ = json.Marshal(conversationMessages[0].Content)
	} else {
		input, _ = json.Marshal(conversationMessages)
	}

	codexReq := &codexRequest{
		Model:        req.Model,
		Input:        input,
		Stream:       false, // set to true for streaming calls
		Temperature:  req.Temperature,
		Instructions: instructions,
		Extra:        make(map[string]json.RawMessage),
	}

	// Map max_tokens → max_output_tokens.
	if req.MaxTokens != nil {
		codexReq.MaxOutputTokens = req.MaxTokens
	}

	// Preserve extra fields from the raw body.
	if len(req.RawBody) > 0 {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(req.RawBody, &raw); err == nil {
			knownFields := map[string]bool{
				"model": true, "messages": true, "stream": true,
				"temperature": true, "max_tokens": true, "max_output_tokens": true,
			}
			for k, v := range raw {
				if !knownFields[k] {
					codexReq.Extra[k] = v
				}
			}

			// Also extract top_p from raw body if present.
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

			chatResp.Choices = append(chatResp.Choices, Choice{
				Index: 0,
				Message: &Message{
					Role:    "assistant",
					Content: content,
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
				Content: "",
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
		Object:  "response",
		Status:  "completed",
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
// If a static model list is configured, it returns those. Otherwise, it returns
// a default set of commonly available Codex models.
func (b *CodexBackend) ListModels(ctx context.Context) ([]Model, error) {
	if len(b.models) > 0 {
		models := make([]Model, 0, len(b.models))
		for _, id := range b.models {
			models = append(models, Model{
				ID:      id,
				Object:  "model",
				Created: time.Now().Unix(),
				OwnedBy: b.name,
			})
		}
		return models, nil
	}

	// Default Codex models.
	defaultModels := []string{
		"o4-mini",
		"gpt-5.2-codex",
		"gpt-5.3-codex",
		"codex-mini",
	}
	models := make([]Model, 0, len(defaultModels))
	for _, id := range defaultModels {
		models = append(models, Model{
			ID:      id,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: b.name,
		})
	}
	return models, nil
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
	source    io.ReadCloser
	scanner   *bufio.Scanner
	responseID string
	modelName  string
	buf       bytes.Buffer
	done      bool
	mu        sync.Mutex

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
					Content: event.Delta,
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
				Index:         0,
				Delta:         &Message{},
				FinishReason:  &finishReason,
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
		} else if token.ExpiresAt.Sub(time.Now()) < 5*time.Minute {
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

	log.Printf("codex backend %s: device code flow initiated, user_code=%s", b.name, resp.UserCode)

	// Start polling in the background — the result will be stored automatically.
	go func() {
		bgCtx := context.Background()
		_, pollErr := b.deviceCodeHandler.WaitForAuthorization(bgCtx, resp)
		if pollErr != nil {
			log.Printf("codex backend %s: device code authorization failed: %v", b.name, pollErr)
		} else {
			log.Printf("codex backend %s: device code authorization completed successfully", b.name)
		}
	}()

	return authURL, state, nil
}

// SupportsDeviceCodeFlow returns true if this backend has a device code handler configured.
func (b *CodexBackend) SupportsDeviceCodeFlow() bool {
	return b.deviceCodeHandler != nil
}
