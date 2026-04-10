package backend

import (
	"context"
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
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatCompletionResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
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
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
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
}
