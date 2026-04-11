package backend

import (
	"context"
	"encoding/json"
	"io"
	"time"
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
	ID              string `json:"id"`
	Object          string `json:"object"`
	Created         int64  `json:"created"`
	OwnedBy         string `json:"owned_by"`
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
}

// ResponsesRequest represents a request to the native Responses API.
// The request body is forwarded as-is (no format translation).
// Only backends implementing ResponsesBackend support this.
type ResponsesRequest struct {
	Model  string          `json:"model"`
	Input  json.RawMessage `json:"input,omitempty"`
	Stream bool            `json:"stream,omitempty"`
	// RawBody preserves the original request body for native passthrough.
	RawBody        []byte `json:"-"`
	APIKeyOverride string `json:"-"`
}

// ResponsesResponse represents a response from the native Responses API.
// The response body is forwarded as-is (no format translation).
type ResponsesResponse struct {
	// Body is the raw JSON response body.
	Body []byte
}

// ResponsesBackend is an optional interface that backends can implement
// to support native Responses API passthrough (no format translation).
// CodexBackend implements this; other backends do not.
type ResponsesBackend interface {
	Backend

	// Responses sends a non-streaming Responses API request natively.
	// The request body is forwarded as-is to the upstream API.
	Responses(ctx context.Context, req *ResponsesRequest) (*ResponsesResponse, error)

	// ResponsesStream sends a streaming Responses API request natively.
	// Returns a reader of raw SSE data from the upstream (no translation).
	ResponsesStream(ctx context.Context, req *ResponsesRequest) (io.ReadCloser, error)
}

// OAuthStatus represents the authentication status of an OAuth-based backend.
type OAuthStatus struct {
	// BackendName is the name of the backend.
	BackendName string `json:"backend_name"`
	// BackendType is the type of the backend (e.g., "copilot", "codex").
	BackendType string `json:"backend_type"`
	// Authenticated is true if the backend has a valid token.
	Authenticated bool `json:"authenticated"`
	// TokenExpiry is the time the current token expires, if available.
	TokenExpiry string `json:"token_expiry,omitempty"`
	// ExpiresAt is the parsed expiry time for computing relative display.
	ExpiresAt time.Time `json:"-"`
	// LastRefresh is when the token was last obtained or refreshed.
	LastRefresh string `json:"last_refresh,omitempty"`
	// ObtainedAt is the parsed time the token was obtained.
	ObtainedAt time.Time `json:"-"`
	// TokenSource is where the token came from (e.g., "env:GH_TOKEN", "codex_oauth").
	TokenSource string `json:"token_source,omitempty"`
	// TokenState is the visual indicator state: "valid", "expiring", "expired", or "missing".
	TokenState string `json:"token_state"`
	// NeedsReauth is true if the token is expired and cannot be refreshed.
	NeedsReauth bool `json:"needs_reauth,omitempty"`
	// ReauthURL is the URL to initiate re-authentication (if needed).
	ReauthURL string `json:"reauth_url,omitempty"`
}

// OAuthStatusProvider is an optional interface that backends can implement
// to expose their current OAuth authentication status. The web UI and health
// endpoint use this to display auth status and support re-authentication.
type OAuthStatusProvider interface {
	// OAuthStatus returns the current authentication status of the backend.
	OAuthStatus() OAuthStatus
}

// OAuthLoginHandler is an optional interface for backends that support
// initiating an OAuth login flow (e.g., Codex with PKCE).
type OAuthLoginHandler interface {
	// InitiateLogin returns a URL to redirect the user to for authentication.
	// The state parameter is used for CSRF protection.
	InitiateLogin() (authURL string, state string, err error)
}

// OAuthCallbackHandler is an optional interface for backends that handle
// OAuth callbacks (e.g., Codex with authorization code exchange).
type OAuthCallbackHandler interface {
	// HandleCallback processes an OAuth callback with the given code and state.
	HandleCallback(ctx context.Context, code string, state string) error
}

// OAuthDisconnectHandler is an optional interface for backends that support
// disconnecting (clearing stored tokens).
type OAuthDisconnectHandler interface {
	// Disconnect clears all stored tokens for the backend.
	Disconnect() error
}

// OAuthDeviceCodeLoginHandler is an optional interface for backends that support
// device code flow as an alternative login method (for headless/SSH environments).
type OAuthDeviceCodeLoginHandler interface {
	// InitiateDeviceCodeLogin starts a device code flow. Like OAuthLoginHandler,
	// it returns a JSON-encoded DeviceCodeLoginInfo as the authURL.
	InitiateDeviceCodeLogin() (authURL string, state string, err error)
}
