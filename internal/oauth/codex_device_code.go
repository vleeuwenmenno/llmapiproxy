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
	// Codex device code flow constants.
	defaultCodexDeviceCodeURL = "https://auth.openai.com/oauth/device/code"

	// codexDeviceHTTPTimeout is the timeout for device code HTTP requests.
	codexDeviceHTTPTimeout = 30 * time.Second

	// codexDefaultPollInterval is the default polling interval in seconds.
	codexDefaultPollInterval = 5

	// codexDeviceCodeExpiry is the default device code expiry in seconds (15 minutes).
	codexDeviceCodeExpiry = 900
)

// CodexDeviceCodeResponse represents the response from the OpenAI device code endpoint.
type CodexDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// CodexDeviceCodeHandler manages the device code flow for OpenAI Codex.
// This provides an alternative login method for headless/SSH environments
// where a browser-based PKCE flow is impractical.
//
// The flow is:
//  1. POST auth.openai.com/oauth/device/code → device_code + user_code + verification_uri
//  2. Display user_code + verification_uri to user (in web UI)
//  3. Poll auth.openai.com/oauth/token with grant_type=urn:ietf:params:oauth:grant-type:device_code
//     until authorized → access_token + refresh_token
//  4. Tokens have the same lifecycle as PKCE tokens (stored in TokenStore, refreshed via refresh_token)
type CodexDeviceCodeHandler struct {
	config *CodexOAuthConfig
	store  *TokenStore
	client *http.Client

	// deviceCodeURL is the endpoint to request a device code.
	deviceCodeURL string

	// mu protects pending flows.
	mu      sync.RWMutex
	pending map[string]*CodexDeviceCodeResponse // keyed by device_code
}

// CodexDeviceCodeHandlerOption configures a CodexDeviceCodeHandler.
type CodexDeviceCodeHandlerOption func(*CodexDeviceCodeHandler)

// WithCodexDeviceCodeURL sets the device code endpoint URL (for testing).
func WithCodexDeviceCodeURL(u string) CodexDeviceCodeHandlerOption {
	return func(h *CodexDeviceCodeHandler) {
		h.deviceCodeURL = u
	}
}

// NewCodexDeviceCodeHandler creates a new CodexDeviceCodeHandler.
func NewCodexDeviceCodeHandler(store *TokenStore, config *CodexOAuthConfig, opts ...CodexDeviceCodeHandlerOption) *CodexDeviceCodeHandler {
	if config == nil {
		config = DefaultCodexOAuthConfig()
	}
	h := &CodexDeviceCodeHandler{
		config:        config,
		store:         store,
		client:        &http.Client{Timeout: codexDeviceHTTPTimeout},
		deviceCodeURL: defaultCodexDeviceCodeURL,
		pending:       make(map[string]*CodexDeviceCodeResponse),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// InitiateDeviceCode initiates a new device code flow by requesting a device code
// from the OpenAI device code endpoint. Returns the response containing the user_code
// and verification_uri that the user must visit.
func (h *CodexDeviceCodeHandler) InitiateDeviceCode(ctx context.Context) (*CodexDeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {h.config.ClientID},
		"scope":     {h.config.Scope},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("codex device code: creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("codex device code: HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("codex device code: reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &errResp) == nil && errResp.Error != "" {
			return nil, &DeviceCodeError{Code: errResp.Error, Description: errResp.ErrorDescription}
		}
		return nil, fmt.Errorf("codex device code: unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var dcResp CodexDeviceCodeResponse
	if err := json.Unmarshal(body, &dcResp); err != nil {
		return nil, fmt.Errorf("codex device code: parsing response: %w", err)
	}

	// Set defaults.
	if dcResp.Interval == 0 {
		dcResp.Interval = codexDefaultPollInterval
	}
	if dcResp.ExpiresIn == 0 {
		dcResp.ExpiresIn = codexDeviceCodeExpiry
	}

	// Track the pending flow.
	h.mu.Lock()
	h.pending[dcResp.DeviceCode] = &dcResp
	h.mu.Unlock()

	log.Printf("codex device code: initiated flow, user_code=%s, verification_uri=%s",
		dcResp.UserCode, dcResp.VerificationURI)

	return &dcResp, nil
}

// WaitForAuthorization polls the token endpoint until the user authorizes the device code.
// On success, the tokens are stored in the TokenStore and the TokenData is returned.
// The tokens have the same lifecycle as PKCE tokens.
func (h *CodexDeviceCodeHandler) WaitForAuthorization(ctx context.Context, dcResp *CodexDeviceCodeResponse) (*TokenData, error) {
	tokenData, err := h.pollForToken(ctx, dcResp.DeviceCode)
	if err != nil {
		return nil, fmt.Errorf("codex device code authorization: %w", err)
	}

	// Clean up the pending flow.
	h.mu.Lock()
	delete(h.pending, dcResp.DeviceCode)
	h.mu.Unlock()

	// Persist the token.
	tokenData.Source = "codex_device_code"
	if err := h.store.Save(tokenData); err != nil {
		log.Printf("warning: failed to persist Codex device code token: %v", err)
	}

	log.Printf("codex device code: successfully authenticated, token expires at %s",
		tokenData.ExpiresAt.Format(time.RFC3339))

	return tokenData, nil
}

// pollForToken polls the token endpoint until the user authorizes the device code.
func (h *CodexDeviceCodeHandler) pollForToken(ctx context.Context, deviceCode string) (*TokenData, error) {
	interval := codexDefaultPollInterval

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("polling cancelled: %w", ctx.Err())
		case <-time.After(time.Duration(interval) * time.Second):
		}

		data := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"client_id":   {h.config.ClientID},
			"device_code": {deviceCode},
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.TokenURL, strings.NewReader(data.Encode()))
		if err != nil {
			return nil, fmt.Errorf("creating token request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := h.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("token HTTP request: %w", err)
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading token response: %w", err)
		}

		// Check for errors.
		var errResult struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if json.Unmarshal(body, &errResult) == nil && errResult.Error != "" {
			switch errResult.Error {
			case "authorization_pending":
				// User hasn't authorized yet, keep polling.
				continue
			case "slow_down":
				// Increase the polling interval by 5 seconds.
				interval += 5
				continue
			case "expired_token":
				return nil, &DeviceCodeError{Code: "expired_token", Description: "The device code has expired. Please try again."}
			case "access_denied":
				return nil, &DeviceCodeError{Code: "access_denied", Description: "Authorization was denied by the user."}
			default:
				return nil, &DeviceCodeError{Code: errResult.Error, Description: errResult.ErrorDescription}
			}
		}

		// Try to parse as successful token response.
		return parseTokenResponse(body)
	}
}

// GetPendingDeviceCode returns the pending device code response for the given device code, or nil.
func (h *CodexDeviceCodeHandler) GetPendingDeviceCode(deviceCode string) *CodexDeviceCodeResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pending[deviceCode]
}

// HasPendingFlow returns true if there's a pending device code flow.
func (h *CodexDeviceCodeHandler) HasPendingFlow() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.pending) > 0
}
