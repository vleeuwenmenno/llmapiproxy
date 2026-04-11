// Package oauth provides OAuth PKCE flow for OpenAI Codex authentication.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// Default Codex OAuth constants (from OpenAI Codex CLI).
	defaultCodexClientID    = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultCodexAuthURL     = "https://auth.openai.com/oauth/authorize"
	defaultCodexTokenURL    = "https://auth.openai.com/oauth/token"
	defaultCodexRedirectURI = "http://localhost:8000/ui/oauth/callback/codex"
	defaultCodexScope       = "openid profile email offline_access"

	// PKCE parameters.
	pkceVerifierLength = 32 // bytes of randomness for code verifier

	// CodexOAuthTimeout is the maximum time to wait for an OAuth callback
	// before cleaning up the pending state.
	codexOAuthTimeout = 10 * time.Minute

	// codexHTTPTimeout is the timeout for token exchange HTTP requests.
	codexHTTPTimeout = 30 * time.Second
)

// PKCEPair holds the PKCE code verifier and challenge.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE creates a PKCE code verifier and S256 code challenge.
// The verifier is a cryptographically random string, and the challenge
// is its SHA-256 hash, both base64url-encoded (no padding).
// The verifier is never logged or persisted to disk.
func GeneratePKCE() (*PKCEPair, error) {
	// Generate random bytes for the verifier.
	verifierBytes := make([]byte, pkceVerifierLength)
	if _, err := rand.Read(verifierBytes); err != nil {
		return nil, fmt.Errorf("generating PKCE verifier: %w", err)
	}

	// Base64url-encode the verifier (no padding).
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)

	// Compute SHA-256 hash of the verifier.
	hash := sha256.Sum256([]byte(verifier))

	// Base64url-encode the hash (no padding) to create the challenge.
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	return &PKCEPair{
		Verifier:  verifier,
		Challenge: challenge,
	}, nil
}

// PendingOAuthState holds the in-memory state for an in-progress OAuth flow.
type PendingOAuthState struct {
	State      string
	PKCE       *PKCEPair
	CreatedAt  time.Time
	RedirectURI string
}

// IsExpired returns true if the OAuth state has expired and should be cleaned up.
func (s *PendingOAuthState) IsExpired() bool {
	return time.Now().After(s.CreatedAt.Add(codexOAuthTimeout))
}

// CodexOAuthConfig holds the configuration for the Codex OAuth flow.
type CodexOAuthConfig struct {
	ClientID    string
	AuthURL     string
	TokenURL    string
	RedirectURI string
	Scope       string
}

// DefaultCodexOAuthConfig returns the default OAuth configuration for Codex.
func DefaultCodexOAuthConfig() *CodexOAuthConfig {
	return &CodexOAuthConfig{
		ClientID:    defaultCodexClientID,
		AuthURL:     defaultCodexAuthURL,
		TokenURL:    defaultCodexTokenURL,
		RedirectURI: defaultCodexRedirectURI,
		Scope:       defaultCodexScope,
	}
}

// CodexOAuthHandler manages the OAuth PKCE flow for OpenAI Codex.
// It handles:
//   - PKCE code verifier/challenge generation (S256)
//   - OAuth authorize URL construction
//   - Callback handling with code exchange
//   - Token refresh with rotation (new refresh token each time)
//   - CSRF state parameter validation
//   - Callback timeout with state cleanup
//
// The PKCE verifier is never logged or persisted to disk.
type CodexOAuthHandler struct {
	config *CodexOAuthConfig
	store  *TokenStore
	client *http.Client

	mu     sync.RWMutex
	pending map[string]*PendingOAuthState // keyed by state string
}

// NewCodexOAuthHandler creates a new CodexOAuthHandler.
func NewCodexOAuthHandler(store *TokenStore, config *CodexOAuthConfig) *CodexOAuthHandler {
	if config == nil {
		config = DefaultCodexOAuthConfig()
	}
	h := &CodexOAuthHandler{
		config:  config,
		store:   store,
		client:  &http.Client{Timeout: codexHTTPTimeout},
		pending: make(map[string]*PendingOAuthState),
	}
	// Start background cleanup of expired states.
	go h.cleanupExpiredStates()
	return h
}

// AuthorizeURL initiates a new OAuth flow by generating a PKCE pair and
// constructing the authorization URL. The PKCE verifier is stored in memory
// only (never logged or persisted to disk). The state parameter provides
// CSRF protection.
//
// Returns the authorization URL to redirect the user to and the state
// parameter (which is also stored internally for callback validation).
func (h *CodexOAuthHandler) AuthorizeURL() (authURL string, state string, err error) {
	// Generate PKCE pair.
	pkce, err := GeneratePKCE()
	if err != nil {
		return "", "", fmt.Errorf("codex oauth: generating PKCE: %w", err)
	}

	// Generate random state for CSRF protection.
	state = uuid.New().String()

	// Store pending state in memory (NOT the token store — the PKCE verifier
	// must never be persisted to disk).
	now := time.Now()
	h.mu.Lock()
	h.pending[state] = &PendingOAuthState{
		State:       state,
		PKCE:        pkce,
		CreatedAt:   now,
		RedirectURI: h.config.RedirectURI,
	}
	h.mu.Unlock()

	// Construct the authorization URL.
	u, err := url.Parse(h.config.AuthURL)
	if err != nil {
		return "", "", fmt.Errorf("codex oauth: parsing auth URL: %w", err)
	}

	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", h.config.ClientID)
	q.Set("redirect_uri", h.config.RedirectURI)
	q.Set("scope", h.config.Scope)
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("originator", "codex_cli_rs")
	u.RawQuery = q.Encode()

	return u.String(), state, nil
}

// HandleCallback processes the OAuth callback by validating the state parameter
// (CSRF protection), exchanging the authorization code for tokens using the
// stored PKCE verifier, and persisting the tokens via the TokenStore.
//
// Returns an error if:
//   - The state parameter is missing or doesn't match any pending flow
//   - The code exchange fails
//   - The token response is malformed
func (h *CodexOAuthHandler) HandleCallback(ctx context.Context, code string, state string) (*TokenData, error) {
	// Validate the state parameter (CSRF protection).
	h.mu.Lock()
	pending, ok := h.pending[state]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("codex oauth callback: invalid or expired state parameter")
	}
	// Remove the pending state immediately (one-time use).
	delete(h.pending, state)
	h.mu.Unlock()

	// Check if the state has expired.
	if pending.IsExpired() {
		return nil, fmt.Errorf("codex oauth callback: authorization flow expired")
	}

	// Exchange the authorization code for tokens.
	// The PKCE verifier is used here and then discarded — it is never logged
	// or persisted to disk.
	tokenData, err := h.exchangeCode(ctx, code, pending.PKCE.Verifier, pending.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("codex oauth callback: %w", err)
	}

	// Persist the token (without the PKCE verifier — it's only in memory).
	if err := h.store.Save(tokenData); err != nil {
		// Log but don't fail — in-memory is still usable.
		log.Printf("warning: failed to persist Codex OAuth token: %v", err)
	}

	return tokenData, nil
}

// exchangeCode sends the authorization code to the token endpoint with the
// PKCE code verifier to exchange it for access and refresh tokens.
func (h *CodexOAuthHandler) exchangeCode(ctx context.Context, code string, verifier string, redirectURI string) (*TokenData, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {h.config.ClientID},
		"code":          {code},
		"code_verifier": {verifier},
		"redirect_uri":  {redirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating token exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (HTTP %d): %s", resp.StatusCode, maskTokenBody(body))
	}

	return parseTokenResponse(body)
}

// RefreshToken refreshes the access token using the stored refresh token.
// If the response includes a new refresh token (rotation), the new token
// is stored and the old one is discarded. This implements token rotation
// per the OAuth 2.0 best practice.
func (h *CodexOAuthHandler) RefreshToken(ctx context.Context) (*TokenData, error) {
	// Get the current token to extract the refresh token.
	current := h.store.Get()
	if current == nil {
		return nil, fmt.Errorf("codex oauth: no token available for refresh")
	}
	if current.RefreshToken == "" {
		return nil, fmt.Errorf("codex oauth: no refresh token available; re-authentication required")
	}

	return h.refreshWithToken(ctx, current.RefreshToken)
}

// RefreshWithRetry attempts to refresh the token. If the current token is
// still valid (not expired), it returns the cached token. If expired, it
// uses the refresh token to get a new one. This is used for transparent
// token refresh during active use.
func (h *CodexOAuthHandler) RefreshWithRetry(ctx context.Context) (*TokenData, error) {
	// Check if the cached token is still usable.
	cached := h.store.Get()
	if cached != nil && !cached.IsExpired() {
		return cached, nil
	}

	// Need to refresh. Use refresh coordination from the TokenStore.
	stillValid, done, err := h.store.StartRefresh()
	if err != nil {
		return nil, fmt.Errorf("codex oauth refresh: %w", err)
	}

	if done == nil {
		// Another refresh is in progress.
		if stillValid {
			return h.store.Get(), nil
		}
		// Wait for the refresh to complete.
		return h.waitForRefresh(ctx)
	}
	defer done()

	// We are the designated refresher.
	newToken, err := h.RefreshToken(ctx)
	if err != nil {
		h.store.SetRefreshError(err)
		// If the stale token is still usable, return it as fallback.
		if stillValid {
			log.Printf("codex oauth refresh failed, using stale token: %v", err)
			return h.store.Get(), nil
		}
		return nil, fmt.Errorf("codex oauth refresh: %w", err)
	}

	return newToken, nil
}

// waitForRefresh polls the store until a valid token becomes available.
func (h *CodexOAuthHandler) waitForRefresh(ctx context.Context) (*TokenData, error) {
	deadline := time.Now().Add(refreshWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("codex oauth: context cancelled while waiting for refresh: %w", ctx.Err())
		default:
		}

		token := h.store.ValidToken()
		if token != nil {
			return token, nil
		}

		time.Sleep(refreshWaitInterval)
	}
	return nil, fmt.Errorf("codex oauth: timed out waiting for token refresh")
}

// refreshWithToken performs the actual token refresh HTTP call.
func (h *CodexOAuthHandler) refreshWithToken(ctx context.Context, refreshToken string) (*TokenData, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {h.config.ClientID},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed (HTTP %d): %s", resp.StatusCode, maskTokenBody(body))
	}

	tokenData, err := parseTokenResponse(body)
	if err != nil {
		return nil, err
	}

	// Persist the new token (with potentially rotated refresh token).
	if err := h.store.Save(tokenData); err != nil {
		log.Printf("warning: failed to persist refreshed Codex token: %v", err)
	}

	return tokenData, nil
}

// cleanupExpiredStates periodically removes expired pending OAuth states
// to prevent memory leaks from abandoned authorization flows.
func (h *CodexOAuthHandler) cleanupExpiredStates() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		h.mu.Lock()
		for state, pending := range h.pending {
			if pending.IsExpired() {
				delete(h.pending, state)
			}
		}
		h.mu.Unlock()
	}
}

// PendingStateCount returns the number of pending OAuth states (for diagnostics).
// Used in testing to verify state cleanup.
func (h *CodexOAuthHandler) PendingStateCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.pending)
}

// GetPendingState returns a copy of the pending state for the given state string,
// or nil if not found. Used for testing.
func (h *CodexOAuthHandler) GetPendingState(state string) *PendingOAuthState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if s, ok := h.pending[state]; ok {
		// Return a copy without the PKCE verifier for safety.
		return &PendingOAuthState{
			State:       s.State,
			PKCE:        nil, // Never expose PKCE verifier outside the handler
			CreatedAt:   s.CreatedAt,
			RedirectURI: s.RedirectURI,
		}
	}
	return nil
}

// SetHTTPClient sets a custom HTTP client (for testing).
func (h *CodexOAuthHandler) SetHTTPClient(client *http.Client) {
	h.client = client
}

// tokenResponse represents the JSON response from the token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token,omitempty"`
}

// parseTokenResponse parses the token endpoint response into a TokenData.
func parseTokenResponse(body []byte) (*TokenData, error) {
	var resp tokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if resp.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}
	if resp.RefreshToken == "" {
		return nil, fmt.Errorf("token response missing refresh_token")
	}
	if resp.ExpiresIn <= 0 {
		return nil, fmt.Errorf("token response missing or invalid expires_in")
	}

	now := time.Now()
	return &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		Scope:        resp.Scope,
		ExpiresAt:    now.Add(time.Duration(resp.ExpiresIn) * time.Second),
		ObtainedAt:   now,
		Source:        "codex_oauth",
	}, nil
}

// maskTokenBody masks sensitive data in token response bodies for error messages.
// This ensures no secrets appear in error logs.
func maskTokenBody(body []byte) string {
	s := string(body)
	// Truncate long responses.
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// DeriveRedirectURI constructs the OAuth redirect URI from the server's listen
// address and the backend name. The listen address may be in the form
// ":8000", "0.0.0.0:8000", "localhost:8000", or any host:port combination.
// The resulting redirect URI has the form:
//
//	http://<host>:<port>/ui/oauth/callback/<backendName>
//
// For ":port" or "0.0.0.0:port" forms, "localhost" is used as the host since
// the redirect must point back to the local machine for the OAuth callback to
// reach the proxy server.
func DeriveRedirectURI(listenAddr, backendName string) string {
	host := "localhost"
	port := "8000"

	if listenAddr == "" {
		listenAddr = ":8000"
	}

	// Split host:port.
	if strings.HasPrefix(listenAddr, ":") {
		// ":8000" form — just a port.
		port = listenAddr[1:]
	} else {
		// "host:port" form.
		lastColon := strings.LastIndex(listenAddr, ":")
		if lastColon > 0 {
			host = listenAddr[:lastColon]
			port = listenAddr[lastColon+1:]
		} else {
			// No colon — treat as just a host (unlikely but defensive).
			host = listenAddr
		}
	}

	// Normalize wildcard addresses to localhost.
	if host == "0.0.0.0" || host == "" {
		host = "localhost"
	}

	return fmt.Sprintf("http://%s:%s/ui/oauth/callback/%s", host, port, backendName)
}

// SetRedirectURI updates the redirect URI used in the OAuth flow.
// This allows the redirect URI to be set after handler creation, based on
// the server's actual listen address and the backend name.
func (h *CodexOAuthHandler) SetRedirectURI(redirectURI string) {
	h.config.RedirectURI = redirectURI
}
