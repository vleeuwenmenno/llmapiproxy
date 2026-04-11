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
// It uses the GitHub Device Code Flow to authenticate users and obtain Copilot
// API tokens. The user visits a GitHub URL, enters a code, and authorizes the
// application. The resulting GitHub token is exchanged for a Copilot API token
// via copilot_internal/v2/token. The subscription is validated immediately
// after login. Tokens are long-lived and validated on-demand (no proactive
// refresh).
//
// Supports Individual, Business, and Enterprise base URL variants.
// Upstream 401 responses trigger re-authentication with a single retry and
// loop prevention (max one retry).
type CopilotBackend struct {
	name               string
	baseURL            string
	models             []string
	client             *http.Client
	deviceCodeHandler  *oauth.DeviceCodeHandler
	tokenStore         *oauth.TokenStore
}

// NewCopilotBackend creates a new CopilotBackend from the given configuration,
// device code handler, and token store.
func NewCopilotBackend(cfg config.BackendConfig, deviceCodeHandler *oauth.DeviceCodeHandler, tokenStore *oauth.TokenStore) *CopilotBackend {
	return &CopilotBackend{
		name:              cfg.Name,
		baseURL:           strings.TrimRight(cfg.BaseURL, "/"),
		models:            cfg.ModelIDs(),
		client:            &http.Client{Timeout: 5 * time.Minute},
		deviceCodeHandler: deviceCodeHandler,
		tokenStore:        tokenStore,
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

// ClearModelCache is a no-op for Copilot backends (no model caching).
func (b *CopilotBackend) ClearModelCache() {}

// ChatCompletion sends a non-streaming chat completion request to the Copilot API.
// It obtains or validates the Copilot token, sets required headers, and forwards
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
		// Force a token refresh by expiring the Copilot token but preserving
		// the GitHub token so GetCopilotToken can re-exchange.
		b.forceExpireToken()
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
		// Force a token refresh by expiring the Copilot token but preserving
		// the GitHub token so GetCopilotToken can re-exchange.
		b.forceExpireToken()
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

// getCopilotToken returns a valid Copilot API token. It uses the device code handler
// to get a cached token or re-validate an expired token. Returns an error if no
// token can be obtained (e.g., user has not completed device code flow).
func (b *CopilotBackend) getCopilotToken(ctx context.Context) (string, error) {
	return b.deviceCodeHandler.GetCopilotToken(ctx)
}

// forceExpireToken marks the current Copilot token as expired while preserving
// the GitHub token for re-exchange. This is used when a 401 response is received
// from the upstream Copilot API, indicating the current token is no longer valid.
func (b *CopilotBackend) forceExpireToken() {
	token := b.tokenStore.Get()
	if token != nil {
		// Preserve the GitHub token, expire the Copilot token.
		b.tokenStore.Save(&oauth.TokenData{
			GitHubToken: token.GitHubToken,
			ExpiresAt:   time.Now().Add(-1 * time.Hour), // expired
			ObtainedAt:  time.Now().Add(-2 * time.Hour),
			Source:      token.Source,
		})
	}
}

// setHeaders sets all required Copilot headers on the HTTP request.
// The APIKeyOverride parameter is intentionally ignored for Copilot backends
// because Copilot uses device code authentication, not configurable API keys.
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
		TokenState:  "missing",
		NeedsReauth: true,
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
		// Compute visual indicator state.
		if token.IsExpired() {
			status.TokenState = "expired"
			// If we have a GitHub token, we can re-validate automatically
			if token.GitHubToken != "" {
				status.NeedsReauth = false // Can auto-revalidate
			}
		} else if token.ExpiresAt.Sub(time.Now()) < 5*time.Minute {
			status.TokenState = "expiring"
			status.NeedsReauth = false
		} else {
			status.TokenState = "valid"
			status.NeedsReauth = false
		}
	}

	return status
}

// --- OAuthLoginHandler interface ---

// DeviceCodeLoginInfo holds the device code flow information returned to the UI.
type DeviceCodeLoginInfo struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
}

// InitiateLogin initiates the GitHub Device Code Flow for Copilot authentication.
// This implements the OAuthLoginHandler interface. Unlike PKCE-based flows,
// the device code flow does NOT return a redirect URL. Instead, it returns a
// JSON string containing the user_code and verification_uri that the UI should
// display to the user.
//
// The returned authURL is a JSON-encoded DeviceCodeLoginInfo that the web
// handler can parse and display in the UI.
func (b *CopilotBackend) InitiateLogin() (authURL string, state string, err error) {
	resp, err := b.deviceCodeHandler.InitiateDeviceCode(context.Background())
	if err != nil {
		return "", "", fmt.Errorf("copilot device code flow: %w", err)
	}

	// Return JSON-encoded device code info as the "auth URL" (the web handler
	// will parse this and display it differently than a redirect).
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

	// Use the device_code as the state (for tracking the pending flow).
	state = resp.DeviceCode
	authURL = string(infoJSON)

	log.Printf("copilot backend %s: device code flow initiated, user_code=%s", b.name, resp.UserCode)

	// Start polling in the background — the result will be stored automatically.
	go func() {
		bgCtx := context.Background()
		_, pollErr := b.deviceCodeHandler.WaitForDeviceAuthorization(bgCtx, resp)
		if pollErr != nil {
			log.Printf("copilot backend %s: device code authorization failed: %v", b.name, pollErr)
		} else {
			log.Printf("copilot backend %s: device code authorization completed successfully", b.name)
		}
	}()

	return authURL, state, nil
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

// GetDeviceCodeHandler returns the device code handler (for testing/status).
func (b *CopilotBackend) GetDeviceCodeHandler() *oauth.DeviceCodeHandler {
	return b.deviceCodeHandler
}

// --- OAuthStatusRefresher interface ---

// RefreshOAuthStatus proactively attempts to validate or refresh the Copilot
// token. It calls GetCopilotToken which will re-exchange the stored GitHub
// token if the Copilot token is expired or missing. This is triggered by
// the "Check Status" button in the web UI.
func (b *CopilotBackend) RefreshOAuthStatus(ctx context.Context) error {
	_, err := b.getCopilotToken(ctx)
	if err != nil {
		return fmt.Errorf("copilot backend %s: token refresh failed: %w", b.name, err)
	}
	return nil
}
