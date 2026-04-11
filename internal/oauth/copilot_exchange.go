package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	// defaultCopilotAPIURL is the default GitHub API base URL for Copilot token exchange.
	defaultCopilotAPIURL = "https://api.github.com"

	// copilotTokenPath is the endpoint for exchanging a GitHub token for a Copilot API token.
	copilotTokenPath = "/copilot_internal/v2/token"

	// copilotExchangeTimeout is the maximum time for a token exchange HTTP request.
	copilotExchangeTimeout = 30 * time.Second

	// editorVersion is the Editor-Version header sent to the GitHub API.
	editorVersion = "llmapiproxy/1.0"

	// editorPluginVersion is the Editor-Plugin-Version header sent to the GitHub API.
	editorPluginVersion = "copilot-proxy/1.0"

	// refreshWaitInterval is the polling interval when waiting for an in-flight
	// refresh to complete.
	refreshWaitInterval = 50 * time.Millisecond

	// refreshWaitTimeout is the maximum time to wait for an in-flight refresh.
	refreshWaitTimeout = 35 * time.Second
)

// copilotTokenResponse represents the JSON response from the GitHub Copilot
// token exchange endpoint (GET /copilot_internal/v2/token).
type copilotTokenResponse struct {
	ExpiresAt int64  `json:"expires_at"` // Unix timestamp
	RefreshIn int    `json:"refresh_in"` // Seconds until proactive refresh
	Token     string `json:"token"`      // The Copilot API token (HMAC-signed)
}

// CopilotExchanger exchanges a GitHub token for a Copilot API token via the
// GitHub API. It uses a TokenStore for persistence and refresh coordination.
type CopilotExchanger struct {
	store  *TokenStore
	apiURL string
	client *http.Client
}

// CopilotExchangerOption configures a CopilotExchanger.
type CopilotExchangerOption func(*CopilotExchanger)

// WithCopilotAPIURL sets the GitHub API base URL (defaults to https://api.github.com).
func WithCopilotAPIURL(url string) CopilotExchangerOption {
	return func(e *CopilotExchanger) {
		e.apiURL = strings.TrimRight(url, "/")
	}
}

// WithCopilotHTTPClient sets a custom HTTP client for the exchange requests.
func WithCopilotHTTPClient(client *http.Client) CopilotExchangerOption {
	return func(e *CopilotExchanger) {
		e.client = client
	}
}

// NewCopilotExchanger creates a new CopilotExchanger that exchanges GitHub tokens
// for Copilot API tokens and persists them to the given TokenStore.
func NewCopilotExchanger(store *TokenStore, opts ...CopilotExchangerOption) *CopilotExchanger {
	e := &CopilotExchanger{
		store:  store,
		apiURL: defaultCopilotAPIURL,
		client: &http.Client{
			Timeout: copilotExchangeTimeout,
		},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Exchange performs a Copilot token exchange: sends the GitHub token to the
// GitHub API to obtain a Copilot API token. The result is stored in the
// TokenStore. Returns the TokenData for the new Copilot token.
func (e *CopilotExchanger) Exchange(ctx context.Context, githubToken string) (*TokenData, error) {
	if strings.TrimSpace(githubToken) == "" {
		return nil, fmt.Errorf("copilot token exchange: GitHub token is required")
	}

	url := e.apiURL + copilotTokenPath

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("copilot token exchange: creating request: %w", err)
	}

	// Set required headers.
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Editor-Version", editorVersion)
	req.Header.Set("Editor-Plugin-Version", editorPluginVersion)
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")
	req.Header.Set("User-Agent", "GitHubCopilot/1.0")
	req.Header.Set("Accept", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("copilot token exchange: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("copilot token exchange: reading response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return e.handleErrorResponse(resp.StatusCode, body)
	}

	var tokenResp copilotTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("copilot token exchange: parsing response JSON: %w", err)
	}

	// Build TokenData from the response.
	now := time.Now()
	tokenData := &TokenData{
		AccessToken: tokenResp.Token,
		TokenType:   "Bearer",
		ExpiresAt:   time.Unix(tokenResp.ExpiresAt, 0),
		RefreshIn:   tokenResp.RefreshIn,
		ObtainedAt:  now,
		Source:       "copilot_exchange",
	}

	// Persist to store.
	if err := e.store.Save(tokenData); err != nil {
		// Log but don't fail — in-memory is still usable.
		log.Printf("warning: failed to persist Copilot token: %v", err)
	}

	return tokenData, nil
}

// handleErrorResponse maps non-200 HTTP responses to descriptive errors.
func (e *CopilotExchanger) handleErrorResponse(statusCode int, body []byte) (*TokenData, error) {
	// Try to extract a message from the response body.
	var errResp struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &errResp)

	switch statusCode {
	case http.StatusUnauthorized:
		msg := "GitHub token is invalid or expired"
		if errResp.Message != "" {
			msg = fmt.Sprintf("%s: %s", msg, errResp.Message)
		}
		return nil, fmt.Errorf("copilot token exchange: %s (HTTP 401)", msg)
	case http.StatusForbidden:
		msg := "access denied"
		if errResp.Message != "" {
			msg = fmt.Sprintf("%s: %s", msg, errResp.Message)
		}
		return nil, fmt.Errorf("copilot token exchange: %s (HTTP 403)", msg)
	case http.StatusTooManyRequests:
		msg := "rate limit exceeded"
		if errResp.Message != "" {
			msg = fmt.Sprintf("%s: %s", msg, errResp.Message)
		}
		return nil, fmt.Errorf("copilot token exchange: %s (HTTP 429)", msg)
	default:
		return nil, fmt.Errorf("copilot token exchange: unexpected status %d: %s",
			statusCode, string(body))
	}
}

// GetOrRefresh returns a valid Copilot token, either from the cache or by
// performing a fresh exchange. It uses the TokenStore's refresh coordination
// to ensure only one exchange happens at a time under concurrent access.
//
// If the cached token is still valid and does not need proactive refresh, it
// is returned immediately. If the token needs refresh, a new exchange is
// attempted. If the exchange fails and the cached token is still unexpired, the
// stale token is returned as a fallback.
//
// When concurrent callers arrive with an expired token, the first caller
// performs the exchange while others wait for it to complete, then return the
// newly refreshed token.
func (e *CopilotExchanger) GetOrRefresh(ctx context.Context, githubToken string) (*TokenData, error) {
	// Check if the cached token is valid and doesn't need refresh.
	cached := e.store.Get()
	if cached != nil && !cached.IsExpired() && !cached.NeedsRefresh() {
		return cached, nil
	}

	// Acquire refresh coordination lock.
	stillValid, done, err := e.store.StartRefresh()
	if err != nil {
		return nil, fmt.Errorf("copilot token refresh: %w", err)
	}

	if done == nil {
		// Another refresh is in progress. Return the cached token if still valid.
		if stillValid {
			return e.store.Get(), nil
		}
		// Token is expired and another refresh is handling it.
		// Poll until the refresh completes or times out.
		return e.waitForRefresh(ctx)
	}
	defer done()

	// We are the designated refresher. Try to exchange.
	newToken, err := e.Exchange(ctx, githubToken)
	if err != nil {
		e.store.SetRefreshError(err)

		// If the stale token is still usable, return it as a fallback.
		if stillValid {
			log.Printf("copilot token refresh failed, using stale token: %v", err)
			return e.store.Get(), nil
		}

		return nil, fmt.Errorf("copilot token refresh: %w", err)
	}

	return newToken, nil
}

// waitForRefresh polls the store until a valid token becomes available
// (placed there by the in-flight refresh) or the context/times out.
func (e *CopilotExchanger) waitForRefresh(ctx context.Context) (*TokenData, error) {
	deadline := time.Now().Add(refreshWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("copilot token: context cancelled while waiting for refresh: %w", ctx.Err())
		default:
		}

		// Check if the token has been refreshed.
		token := e.store.ValidToken()
		if token != nil {
			return token, nil
		}

		// Check if the refresh is still in progress by trying to acquire the lock.
		_, done, _ := e.store.StartRefresh()
		if done != nil {
			// The previous refresh finished (no one is refreshing now), but
			// the token is still not valid. Release the lock and return an error.
			done()
			return nil, fmt.Errorf("copilot token: token refresh completed but token is still invalid")
		}

		time.Sleep(refreshWaitInterval)
	}

	return nil, fmt.Errorf("copilot token: timed out waiting for token refresh")
}
