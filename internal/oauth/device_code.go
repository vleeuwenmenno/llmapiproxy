package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// Default GitHub Device Code Flow constants.
	defaultDeviceCodeClientID = "Iv1.b507a08c87ecfe98" // GitHub Copilot VS Code extension client ID
	defaultDeviceCodeURL      = "https://github.com/login/device/code"
	defaultAccessTokenURL     = "https://github.com/login/oauth/access_token"

	// Default Copilot token exchange endpoint.
	defaultCopilotTokenURL = "https://api.github.com/copilot_internal/v2/token"

	// Default polling interval (seconds) if not specified by GitHub.
	defaultPollInterval = 5

	// Default device code expiry (seconds).
	defaultDeviceCodeExpiry = 900 // 15 minutes

	// deviceHTTPTimeout is the timeout for device code HTTP requests.
	deviceHTTPTimeout = 30 * time.Second
)

// DeviceCodeResponse represents the response from the GitHub device code endpoint.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// DeviceCodeError represents an error from the device code flow.
type DeviceCodeError struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *DeviceCodeError) Error() string {
	return fmt.Sprintf("device code error: %s: %s", e.Code, e.Description)
}

// PendingDeviceFlow holds the state for an in-progress device code flow.
type PendingDeviceFlow struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
	CreatedAt       time.Time
}

// IsExpired returns true if the device code has expired.
func (p *PendingDeviceFlow) IsExpired() bool {
	return time.Now().After(p.ExpiresAt)
}

// DeviceCodeHandler manages the GitHub Device Code Flow for Copilot authentication.
//
// The flow is:
//  1. POST github.com/login/device/code → device_code + user_code + verification_uri
//  2. Display user_code + verification_uri to user (in web UI)
//  3. Poll POST github.com/login/oauth/access_token until authorized → GitHub access token
//  4. Exchange GitHub token for Copilot token via copilot_internal/v2/token
//  5. Validate Copilot subscription by verifying the exchange succeeded
//  6. Token is long-lived, validated on-demand (no proactive refresh)
type DeviceCodeHandler struct {
	store     *TokenStore
	exchanger *CopilotExchanger
	client    *http.Client

	// Configurable endpoints (for testing).
	deviceCodeURL  string
	accessTokenURL string
	clientID       string

	// Pending flows tracked for status display.
	mu     sync.RWMutex
	pending map[string]*PendingDeviceFlow // keyed by device_code
}

// DeviceCodeHandlerOption configures a DeviceCodeHandler.
type DeviceCodeHandlerOption func(*DeviceCodeHandler)

// WithDeviceCodeURL sets the device code endpoint URL (for testing).
func WithDeviceCodeURL(url string) DeviceCodeHandlerOption {
	return func(h *DeviceCodeHandler) {
		h.deviceCodeURL = url
	}
}

// WithAccessTokenURL sets the access token endpoint URL (for testing).
func WithAccessTokenURL(url string) DeviceCodeHandlerOption {
	return func(h *DeviceCodeHandler) {
		h.accessTokenURL = url
	}
}

// WithCopilotExchangerURL sets the Copilot token exchange URL (for testing).
// This creates a new CopilotExchanger pointing to the given URL.
func WithCopilotExchangerURL(url string) DeviceCodeHandlerOption {
	return func(h *DeviceCodeHandler) {
		h.exchanger = NewCopilotExchanger(h.store, WithCopilotAPIURL(url))
	}
}

// WithDeviceCodeClientID sets the GitHub OAuth client ID.
func WithDeviceCodeClientID(id string) DeviceCodeHandlerOption {
	return func(h *DeviceCodeHandler) {
		h.clientID = id
	}
}

// NewDeviceCodeHandler creates a new DeviceCodeHandler.
func NewDeviceCodeHandler(store *TokenStore, opts ...DeviceCodeHandlerOption) *DeviceCodeHandler {
	h := &DeviceCodeHandler{
		store:          store,
		exchanger:      NewCopilotExchanger(store),
		client:         &http.Client{Timeout: deviceHTTPTimeout},
		deviceCodeURL:  defaultDeviceCodeURL,
		accessTokenURL: defaultAccessTokenURL,
		clientID:       defaultDeviceCodeClientID,
		pending:        make(map[string]*PendingDeviceFlow),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// InitiateDeviceCode initiates a new device code flow by requesting a device code
// from GitHub. Returns the DeviceCodeResponse containing the user_code and
// verification_uri that the user must visit.
func (h *DeviceCodeHandler) InitiateDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {h.clientID},
		"scope":     {""}, // No additional scopes needed for Copilot
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("device code: creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("device code: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Try to parse as error response
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, &DeviceCodeError{Code: errResp.Error, Description: errResp.ErrorDescription}
		}
		return nil, fmt.Errorf("device code: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var dcResp DeviceCodeResponse
	if err := json.Unmarshal(body, &dcResp); err != nil {
		return nil, fmt.Errorf("device code: parsing response: %w", err)
	}

	// Set defaults
	if dcResp.Interval == 0 {
		dcResp.Interval = defaultPollInterval
	}
	if dcResp.ExpiresIn == 0 {
		dcResp.ExpiresIn = defaultDeviceCodeExpiry
	}

	// Track the pending flow
	now := time.Now()
	h.mu.Lock()
	h.pending[dcResp.DeviceCode] = &PendingDeviceFlow{
		DeviceCode:      dcResp.DeviceCode,
		UserCode:        dcResp.UserCode,
		VerificationURI: dcResp.VerificationURI,
		ExpiresAt:       now.Add(time.Duration(dcResp.ExpiresIn) * time.Second),
		CreatedAt:       now,
	}
	h.mu.Unlock()

	log.Printf("device code: initiated flow, user_code=%s, verification_uri=%s",
		dcResp.UserCode, dcResp.VerificationURI)

	return &dcResp, nil
}

// WaitForDeviceAuthorization polls GitHub until the user authorizes the device code,
// then exchanges the resulting GitHub token for a Copilot token, validates the
// subscription, and stores the token. This is a blocking call.
func (h *DeviceCodeHandler) WaitForDeviceAuthorization(ctx context.Context, dcResp *DeviceCodeResponse) (*TokenData, error) {
	// Poll for the GitHub access token
	githubToken, err := h.pollForAccessToken(ctx, dcResp.DeviceCode)
	if err != nil {
		return nil, fmt.Errorf("device code authorization: %w", err)
	}

	// Exchange GitHub token for Copilot token and validate subscription
	tokenData, err := h.exchangeAndValidate(ctx, githubToken)
	if err != nil {
		return nil, fmt.Errorf("device code exchange: %w", err)
	}

	// Clean up the pending flow
	h.mu.Lock()
	delete(h.pending, dcResp.DeviceCode)
	h.mu.Unlock()

	return tokenData, nil
}

// pollForAccessToken polls the GitHub access token endpoint until the user
// authorizes the device code. Returns the GitHub access token on success.
func (h *DeviceCodeHandler) pollForAccessToken(ctx context.Context, deviceCode string) (string, error) {
	interval := defaultPollInterval

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("polling cancelled: %w", ctx.Err())
		case <-time.After(time.Duration(interval) * time.Second):
		}

		data := url.Values{
			"client_id":   {h.clientID},
			"device_code": {deviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.accessTokenURL, strings.NewReader(data.Encode()))
		if err != nil {
			return "", fmt.Errorf("creating access token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := h.client.Do(req)
		if err != nil {
			return "", fmt.Errorf("access token HTTP request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", fmt.Errorf("reading access token response: %w", err)
		}

		var result struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			Scope       string `json:"scope"`
			Error       string `json:"error"`
			ErrorDesc   string `json:"error_description"`
			Interval    int    `json:"interval"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return "", fmt.Errorf("parsing access token response: %w", err)
		}

		switch {
		case result.Error == "":
			// Success! We got an access token.
			return result.AccessToken, nil
		case result.Error == "authorization_pending":
			// User hasn't authorized yet, keep polling.
		case result.Error == "slow_down":
			// Increase the polling interval by 5 seconds.
			interval += 5
		case result.Error == "expired_token":
			return "", &DeviceCodeError{Code: "expired_token", Description: "The device code has expired. Please try again."}
		case result.Error == "access_denied":
			return "", &DeviceCodeError{Code: "access_denied", Description: "Authorization was denied by the user."}
		default:
			return "", &DeviceCodeError{Code: result.Error, Description: result.ErrorDesc}
		}
	}
}

// exchangeAndValidate exchanges a GitHub token for a Copilot token via the
// GitHub API, validates the subscription, and persists the token.
// The exchange itself is the validation — a successful response means the
// user has an active Copilot subscription.
func (h *DeviceCodeHandler) exchangeAndValidate(ctx context.Context, githubToken string) (*TokenData, error) {
	// The CopilotExchanger handles the exchange and persistence.
	// A successful exchange implicitly validates the Copilot subscription
	// because GitHub only returns a Copilot token for active subscribers.
	tokenData, err := h.exchanger.Exchange(ctx, githubToken)
	if err != nil {
		return nil, fmt.Errorf("Copilot token exchange failed (no active subscription?): %w", err)
	}

	// Store the GitHub token alongside the Copilot token for re-validation.
	// The GitHub token from device code flow is long-lived.
	tokenData.Source = "device_code_flow"
	tokenData.GitHubToken = githubToken

	if err := h.store.Save(tokenData); err != nil {
		log.Printf("warning: failed to persist Copilot token after device code flow: %v", err)
	}

	log.Printf("device code: successfully authenticated Copilot, token expires at %s",
		tokenData.ExpiresAt.Format(time.RFC3339))

	return tokenData, nil
}

// GetCopilotToken returns a valid Copilot API token. It first checks the
// token store for a cached (non-expired) token. If the token is expired,
// it attempts to re-exchange the stored GitHub token for a new Copilot token
// (on-demand validation, no proactive refresh).
func (h *DeviceCodeHandler) GetCopilotToken(ctx context.Context) (string, error) {
	// Check for a valid cached token.
	cached := h.store.ValidToken()
	if cached != nil {
		return cached.AccessToken, nil
	}

	// Token is expired or missing. Try to re-exchange using stored GitHub token.
	stored := h.store.Get()
	if stored == nil {
		return "", fmt.Errorf("no Copilot token available; initiate device code flow via the web UI")
	}

	if stored.GitHubToken == "" {
		return "", fmt.Errorf("Copilot token expired and no GitHub token stored for re-exchange; please re-authenticate via the web UI")
	}

	// Re-exchange the GitHub token for a fresh Copilot token.
	log.Printf("copilot: token expired, re-exchanging GitHub token for fresh Copilot token")
	newToken, err := h.exchangeAndValidate(ctx, stored.GitHubToken)
	if err != nil {
		return "", fmt.Errorf("Copilot token re-exchange failed: %w", err)
	}

	return newToken.AccessToken, nil
}

// GetPendingFlow returns the pending device flow for the given device code,
// or nil if not found. Used for status display in the UI.
func (h *DeviceCodeHandler) GetPendingFlow(deviceCode string) *PendingDeviceFlow {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pending[deviceCode]
}

// GetPendingFlowByUserCode returns the pending device flow for the given user code,
// or nil if not found.
func (h *DeviceCodeHandler) GetPendingFlowByUserCode(userCode string) *PendingDeviceFlow {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, flow := range h.pending {
		if flow.UserCode == userCode {
			return flow
		}
	}
	return nil
}

// HasPendingFlow returns true if there's any pending device code flow.
func (h *DeviceCodeHandler) HasPendingFlow() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.pending) > 0
}
