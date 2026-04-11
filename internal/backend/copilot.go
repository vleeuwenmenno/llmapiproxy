package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
)

const (
	// copilotEditorVersion is the Editor-Version header sent to the Copilot API.
	copilotEditorVersion = "llmapiproxy/1.0"

	// copilotEditorPluginVersion is the Editor-Plugin-Version header.
	copilotEditorPluginVersion = "copilot-proxy/1.0"

	// copilotIntegrationID is the Copilot-Integration-Id header.
	copilotIntegrationID = "vscode-chat"

	// copilotUserAgent is the User-Agent header.
	copilotUserAgent = "GitHubCopilot/1.0"

	// maxAuthRetries is the maximum number of re-authentication attempts on 401.
	// After one retry, we stop to prevent infinite loops (VAL-TOKEN-040).
	maxAuthRetries = 1
)

// CopilotBackend implements the Backend interface for GitHub Copilot.
// It discovers GitHub tokens from the local environment, exchanges them for
// Copilot API tokens, and forwards requests to api.githubcopilot.com (or
// Business/Enterprise variants) with the required Copilot headers.
//
// Supports Individual, Business, and Enterprise base URL variants.
// Upstream 401 responses trigger re-authentication with a single retry and
// loop prevention (max one retry).
type CopilotBackend struct {
	name       string
	baseURL    string
	models     []string
	client     *http.Client
	discoverer *oauth.Discoverer
	exchanger  *oauth.CopilotExchanger
	tokenStore *oauth.TokenStore
}

// NewCopilotBackend creates a new CopilotBackend from the given configuration,
// token discoverer, token exchanger, and token store.
func NewCopilotBackend(cfg config.BackendConfig, discoverer *oauth.Discoverer, exchanger *oauth.CopilotExchanger, tokenStore *oauth.TokenStore) *CopilotBackend {
	return &CopilotBackend{
		name:       cfg.Name,
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		models:     cfg.Models,
		client:     &http.Client{Timeout: 5 * time.Minute},
		discoverer: discoverer,
		exchanger:  exchanger,
		tokenStore: tokenStore,
	}
}

// Name returns the backend's configured name (used as model prefix).
func (b *CopilotBackend) Name() string { return b.name }

// SupportsModel returns true if this backend can handle the given model ID.
// If no models are configured, all models are accepted.
func (b *CopilotBackend) SupportsModel(modelID string) bool {
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

// ChatCompletion sends a non-streaming chat completion request to the Copilot API.
// It discovers or refreshes the Copilot token, sets required headers, and forwards
// the request. If the upstream returns 401, it re-authenticates and retries once.
func (b *CopilotBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return b.doChatCompletion(ctx, req, 0)
}

// doChatCompletion implements ChatCompletion with retry count for 401 loop prevention.
func (b *CopilotBackend) doChatCompletion(ctx context.Context, req *ChatCompletionRequest, retryCount int) (*ChatCompletionResponse, error) {
	// Get a valid Copilot token.
	copilotToken, err := b.getCopilotToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("copilot backend %s: failed to get token: %w", b.name, err)
	}

	body := b.rewriteBody(req)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, copilotToken)

	resp, err := b.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < maxAuthRetries {
		// Force a token refresh by clearing the cached token.
		b.tokenStore.Clear()
		return b.doChatCompletion(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("copilot backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// ChatCompletionStream sends a streaming chat completion request to the Copilot API.
// It returns a reader of SSE data. Upstream 401 triggers re-auth with a single retry.
func (b *CopilotBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	return b.doChatCompletionStream(ctx, req, 0)
}

// doChatCompletionStream implements ChatCompletionStream with retry count for 401 loop prevention.
func (b *CopilotBackend) doChatCompletionStream(ctx context.Context, req *ChatCompletionRequest, retryCount int) (io.ReadCloser, error) {
	// Get a valid Copilot token.
	copilotToken, err := b.getCopilotToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("copilot backend %s: failed to get token: %w", b.name, err)
	}

	body := b.rewriteBody(req)
	// For streaming, don't use the client timeout — the stream can last a long time.
	client := &http.Client{}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	b.setHeaders(httpReq, copilotToken)

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}

	// Handle 401 with re-auth retry.
	if resp.StatusCode == http.StatusUnauthorized && retryCount < maxAuthRetries {
		resp.Body.Close()
		b.tokenStore.Clear()
		return b.doChatCompletionStream(ctx, req, retryCount+1)
	}

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &BackendError{
			StatusCode: resp.StatusCode,
			Body:       string(errBody),
			Err:        fmt.Errorf("copilot backend %s returned status %d: %s", b.name, resp.StatusCode, string(errBody)),
		}
	}

	return resp.Body, nil
}

// ListModels returns the list of models this Copilot backend supports.
// If a static model list is configured, it returns those. Otherwise, it returns
// a default set of commonly available Copilot models.
func (b *CopilotBackend) ListModels(ctx context.Context) ([]Model, error) {
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

	// Default Copilot models.
	defaultModels := []string{
		"gpt-4o",
		"gpt-4.1",
		"o3",
		"o3-mini",
		"o4-mini",
		"claude-sonnet-4",
		"gpt-4o-mini",
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

// getCopilotToken returns a valid Copilot API token. It first checks the token
// store for a cached token, then falls back to discovering a GitHub token and
// exchanging it for a Copilot token. Returns an error if no token can be obtained.
func (b *CopilotBackend) getCopilotToken(ctx context.Context) (string, error) {
	// Check for a valid cached token.
	cached := b.tokenStore.ValidToken()
	if cached != nil {
		return cached.AccessToken, nil
	}

	// Discover a GitHub token.
	githubToken, source, err := b.discoverer.DiscoverGitHubToken()
	if err != nil {
		return "", fmt.Errorf("GitHub token discovery failed: %w", err)
	}
	if githubToken == "" {
		return "", fmt.Errorf("no GitHub token found for Copilot authentication; set COPILOT_GITHUB_TOKEN, GH_TOKEN, or GITHUB_TOKEN env var, or authenticate with gh CLI")
	}

	log.Printf("copilot backend %s: discovered GitHub token from %s", b.name, source)

	// Exchange GitHub token for Copilot token.
	tokenData, err := b.exchanger.GetOrRefresh(ctx, githubToken)
	if err != nil {
		return "", fmt.Errorf("Copilot token exchange failed: %w", err)
	}

	return tokenData.AccessToken, nil
}

// setHeaders sets all required Copilot headers on the HTTP request.
// The APIKeyOverride parameter is intentionally ignored for Copilot backends
// because Copilot uses local GitHub authentication, not configurable API keys.
func (b *CopilotBackend) setHeaders(httpReq *http.Request, copilotToken string) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+copilotToken)
	httpReq.Header.Set("Editor-Version", copilotEditorVersion)
	httpReq.Header.Set("Editor-Plugin-Version", copilotEditorPluginVersion)
	httpReq.Header.Set("Copilot-Integration-Id", copilotIntegrationID)
	httpReq.Header.Set("User-Agent", copilotUserAgent)
	httpReq.Header.Set("Accept", "application/json")

	// Generate a unique request ID for tracing.
	requestID := uuid.New().String()
	httpReq.Header.Set("X-Request-Id", requestID)
}

// rewriteBody rewrites the request body, replacing the model field with the
// (prefix-stripped) model ID from the request. Extra fields are preserved.
func (b *CopilotBackend) rewriteBody(req *ChatCompletionRequest) []byte {
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

// --- OAuthStatusProvider interface ---

// OAuthStatus returns the current authentication status of the Copilot backend.
func (b *CopilotBackend) OAuthStatus() OAuthStatus {
	status := OAuthStatus{
		BackendName: b.name,
		BackendType: "copilot",
	}

	token := b.tokenStore.Get()
	if token != nil {
		status.Authenticated = !token.IsExpired()
		status.TokenSource = token.Source
		if !token.ExpiresAt.IsZero() {
			status.TokenExpiry = token.ExpiresAt.Format(time.RFC3339)
		}
	}

	return status
}

// --- OAuthDisconnectHandler interface ---

// Disconnect clears all stored tokens for the Copilot backend.
func (b *CopilotBackend) Disconnect() error {
	return b.tokenStore.Clear()
}

// GetTokenStore returns the underlying TokenStore (for status checking).
func (b *CopilotBackend) GetTokenStore() *oauth.TokenStore {
	return b.tokenStore
}
