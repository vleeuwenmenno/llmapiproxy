package backend

import (
	"context"
	"encoding/json"
	"io"
)

// ChatCompletionRequest is the OpenAI-compatible chat completion request.
type ChatCompletionRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	// RawBody preserves the original request body for passthrough.
	RawBody        []byte `json:"-"`
	APIKeyOverride string `json:"-"`
}

// BackendError wraps a non-2xx backend response with its status code and body.
type BackendError struct {
	StatusCode int
	Body       string
	Err        error
}

func (e *BackendError) Error() string { return e.Err.Error() }
func (e *BackendError) Unwrap() error { return e.Err }

// RouteEntry pairs a resolved backend with the model ID to forward.
type RouteEntry struct {
	Backend Backend
	ModelID string
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
	// RawBody preserves the original upstream response for transparent passthrough.
	RawBody []byte `json:"-"`
}

type Choice struct {
	Index        int      `json:"index"`
	Message      *Message `json:"message,omitempty"`
	Delta        *Message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// PromptTokensDetails holds provider-specific prompt token breakdown.
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// CompletionTokensDetails holds provider-specific completion token breakdown.
	CompletionTokensDetails *CompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

// PromptTokensDetails provides a breakdown of prompt token usage.
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
	AudioTokens  int `json:"audio_tokens,omitempty"`
}

// CompletionTokensDetails provides a breakdown of completion token usage.
type CompletionTokensDetails struct {
	ReasoningTokens          int `json:"reasoning_tokens"`
	AudioTokens              int `json:"audio_tokens,omitempty"`
	AcceptedPredictionTokens int `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int `json:"rejected_prediction_tokens,omitempty"`
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// DisplayName is a human-readable model name, e.g. "Claude Sonnet 4".
	// Empty when not known; clients should fall back to deriving a name from ID.
	DisplayName     string `json:"display_name,omitempty"`
	ContextLength   *int64 `json:"context_length,omitempty"`
	MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
	// Capabilities lists supported features, e.g. ["vision", "tools"].
	Capabilities []string `json:"capabilities,omitempty"`
}

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// Backend is the interface that all LLM backends must implement.
type Backend interface {
	// Name returns the backend's configured name (used as model prefix).
	Name() string

	// ChatCompletion sends a non-streaming chat completion request.
	// The model name in the request has already been stripped of the backend prefix.
	ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error)

	// ChatCompletionStream sends a streaming chat completion request.
	// Returns a reader of SSE data that should be piped to the client.
	ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error)

	// ListModels returns the list of models this backend supports.
	ListModels(ctx context.Context) ([]Model, error)

	// SupportsModel returns true if this backend can handle the given model ID
	// (without the backend prefix).
	SupportsModel(modelID string) bool

	// ClearModelCache clears the cached model list, forcing a fresh fetch
	// on the next ListModels call.
	ClearModelCache()
}
