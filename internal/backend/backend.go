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
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	ToolCalls json.RawMessage `json:"tool_calls,omitempty"`
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
	// AvailableBackends lists the backends that serve this model, in routing priority order.
	// Set by the /v1/models handler when flattening across multiple backends.
	AvailableBackends []string `json:"available_backends,omitempty"`
	// RoutingStrategy is the effective routing strategy for this model
	// (e.g. "priority", "round-robin", "race", "staggered-race").
	RoutingStrategy string `json:"routing_strategy,omitempty"`
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

	// ResolveModelID maps a canonical model ID (e.g. "minimax-m2.7") to the
	// backend-specific model ID (e.g. "minimaxai/minimax-m2.7"). If no mapping
	// is found, the input is returned unchanged.
	ResolveModelID(canonicalID string) string

	// ClearModelCache clears the cached model list, forcing a fresh fetch
	// on the next ListModels call.
	ClearModelCache()
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

// OAuthStatusRefresher is an optional interface for backends that support
// proactively refreshing their OAuth token status. When implemented, the
// web UI's "Check Status" button will trigger this method to attempt a
// fresh token exchange or validation before returning status.
type OAuthStatusRefresher interface {
	// RefreshOAuthStatus attempts to obtain a fresh token (re-exchange if
	// expired, re-validate if cached). Returns an error if the refresh fails
	// (e.g., no GitHub token available, network error).
	RefreshOAuthStatus(ctx context.Context) error
}

// UpstreamModelsResponse contains the raw HTTP response from an upstream
// /models endpoint, used for debugging model listing issues.
type UpstreamModelsResponse struct {
	Backend     string `json:"backend"`
	URL         string `json:"url"`
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type,omitempty"`
	RawBody     string `json:"raw_body"`
	Error       string `json:"error,omitempty"`
}

// UpstreamModelsProvider is an optional interface that dynamic backends
// can implement to expose the raw upstream /models API response for debugging.
type UpstreamModelsProvider interface {
	FetchUpstreamModelsRaw(ctx context.Context) (*UpstreamModelsResponse, error)
}
