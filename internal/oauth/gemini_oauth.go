package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	log "github.com/rs/zerolog/log"
)

// DefaultGeminiClientID and DefaultGeminiClientSecret are the public OAuth
// credentials from the upstream Gemini CLI open-source project. They are
// intentionally not stored here; configure them via config.yaml oauth section
// or set GEMINI_CLIENT_ID / GEMINI_CLIENT_SECRET environment variables.
// See config.example.yaml for guidance.
const (
	defaultGeminiAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	defaultGeminiTokenURL = "https://oauth2.googleapis.com/token"
	geminiOAuthTimeout    = 10 * time.Minute
	geminiHTTPTimeout     = 30 * time.Second
)

var defaultGeminiScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
}

type GeminiOAuthConfig struct {
	ClientID     string
	ClientSecret string
	AuthURL      string
	TokenURL     string
	RedirectURI  string
	Scopes       []string
}

func DefaultGeminiOAuthConfig() *GeminiOAuthConfig {
	return &GeminiOAuthConfig{
		// ClientID and ClientSecret must be set via config.yaml.
		// The upstream Gemini CLI credentials are documented in config.example.yaml.
		AuthURL:     defaultGeminiAuthURL,
		TokenURL:    defaultGeminiTokenURL,
		RedirectURI: "http://127.0.0.1:42857/oauth2callback",
		Scopes:      defaultGeminiScopes,
	}
}

type PendingGeminiOAuthState struct {
	State     string
	CreatedAt time.Time
}

func (s *PendingGeminiOAuthState) IsExpired() bool {
	return time.Now().After(s.CreatedAt.Add(geminiOAuthTimeout))
}

type GeminiOAuthHandler struct {
	config *GeminiOAuthConfig
	store  *TokenStore
	client *http.Client

	mu      sync.RWMutex
	pending map[string]*PendingGeminiOAuthState
}

func NewGeminiOAuthHandler(store *TokenStore, config *GeminiOAuthConfig) *GeminiOAuthHandler {
	if config == nil {
		config = DefaultGeminiOAuthConfig()
	}
	h := &GeminiOAuthHandler{
		config:  config,
		store:   store,
		client:  &http.Client{Timeout: geminiHTTPTimeout},
		pending: make(map[string]*PendingGeminiOAuthState),
	}
	go h.cleanupExpiredStates()
	return h
}

func (h *GeminiOAuthHandler) AuthorizeURL() (authURL string, state string, err error) {
	state = uuid.New().String()

	now := time.Now()
	h.mu.Lock()
	h.pending[state] = &PendingGeminiOAuthState{
		State:     state,
		CreatedAt: now,
	}
	h.mu.Unlock()

	u, err := url.Parse(h.config.AuthURL)
	if err != nil {
		return "", "", fmt.Errorf("gemini oauth: parsing auth URL: %w", err)
	}

	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", h.config.ClientID)
	q.Set("redirect_uri", h.config.RedirectURI)
	q.Set("scope", strings.Join(h.config.Scopes, " "))
	q.Set("state", state)
	q.Set("access_type", "offline")
	u.RawQuery = q.Encode()

	return u.String(), state, nil
}

func (h *GeminiOAuthHandler) HandleCallback(ctx context.Context, code string, state string) (*TokenData, error) {
	h.mu.Lock()
	pending, ok := h.pending[state]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("gemini oauth callback: invalid or expired state parameter")
	}
	delete(h.pending, state)
	h.mu.Unlock()

	if pending.IsExpired() {
		return nil, fmt.Errorf("gemini oauth callback: authorization flow expired")
	}

	tokenData, err := h.exchangeCode(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("gemini oauth callback: %w", err)
	}

	if err := h.store.Save(tokenData); err != nil {
		log.Warn().Err(err).Msg("failed to persist Gemini OAuth token")
	}

	return tokenData, nil
}

func (h *GeminiOAuthHandler) exchangeCode(ctx context.Context, code string) (*TokenData, error) {
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {h.config.ClientID},
		"client_secret": {h.config.ClientSecret},
		"code":          {code},
		"redirect_uri":  {h.config.RedirectURI},
	}

	return h.doTokenRequest(ctx, data)
}

func (h *GeminiOAuthHandler) RefreshToken(ctx context.Context) (*TokenData, error) {
	current := h.store.Get()
	if current == nil {
		return nil, fmt.Errorf("gemini oauth: no token available for refresh")
	}
	if current.RefreshToken == "" {
		return nil, fmt.Errorf("gemini oauth: no refresh token available; re-authentication required")
	}

	return h.refreshWithToken(ctx, current.RefreshToken)
}

func (h *GeminiOAuthHandler) RefreshWithRetry(ctx context.Context) (*TokenData, error) {
	cached := h.store.Get()
	if cached != nil && !cached.IsExpired() {
		return cached, nil
	}

	stillValid, done, err := h.store.StartRefresh()
	if err != nil {
		return nil, fmt.Errorf("gemini oauth refresh: %w", err)
	}

	if done == nil {
		if stillValid {
			return h.store.Get(), nil
		}
		return h.waitForRefresh(ctx)
	}
	defer done()

	newToken, err := h.RefreshToken(ctx)
	if err != nil {
		h.store.SetRefreshError(err)
		if stillValid {
			log.Warn().Err(err).Msg("gemini oauth refresh failed, using stale token")
			return h.store.Get(), nil
		}
		return nil, fmt.Errorf("gemini oauth refresh: %w", err)
	}

	return newToken, nil
}

func (h *GeminiOAuthHandler) waitForRefresh(ctx context.Context) (*TokenData, error) {
	deadline := time.Now().Add(refreshWaitTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("gemini oauth: context cancelled while waiting for refresh: %w", ctx.Err())
		default:
		}

		token := h.store.ValidToken()
		if token != nil {
			return token, nil
		}

		time.Sleep(refreshWaitInterval)
	}
	return nil, fmt.Errorf("gemini oauth: timed out waiting for token refresh")
}

func (h *GeminiOAuthHandler) refreshWithToken(ctx context.Context, refreshToken string) (*TokenData, error) {
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {h.config.ClientID},
		"client_secret": {h.config.ClientSecret},
	}

	tokenData, err := h.doTokenRequest(ctx, data)
	if err != nil {
		return nil, err
	}

	if tokenData.RefreshToken == "" {
		tokenData.RefreshToken = refreshToken
	}

	if err := h.store.Save(tokenData); err != nil {
		log.Warn().Err(err).Msg("failed to persist refreshed Gemini token")
	}

	return tokenData, nil
}

func (h *GeminiOAuthHandler) doTokenRequest(ctx context.Context, data url.Values) (*TokenData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.config.TokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token request failed (HTTP %d): %s", resp.StatusCode, maskTokenBody(body))
	}

	return h.parseTokenResponse(body)
}

type geminiTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	IDToken      string `json:"id_token,omitempty"`
}

func (h *GeminiOAuthHandler) parseTokenResponse(body []byte) (*TokenData, error) {
	var resp geminiTokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	if resp.AccessToken == "" {
		return nil, fmt.Errorf("token response missing access_token")
	}

	now := time.Now()
	td := &TokenData{
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		Scope:        resp.Scope,
		ExpiresAt:    now.Add(time.Duration(resp.ExpiresIn) * time.Second),
		ObtainedAt:   now,
		Source:       "gemini_oauth",
	}
	if resp.ExpiresIn <= 0 {
		td.ExpiresAt = now.Add(1 * time.Hour)
	}

	return td, nil
}

func (h *GeminiOAuthHandler) cleanupExpiredStates() {
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

func (h *GeminiOAuthHandler) PendingStateCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.pending)
}

func (h *GeminiOAuthHandler) GetPendingState(state string) *PendingGeminiOAuthState {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.pending[state]
}

func (h *GeminiOAuthHandler) SetHTTPClient(client *http.Client) {
	h.client = client
}

func (h *GeminiOAuthHandler) SetRedirectURI(redirectURI string) {
	h.config.RedirectURI = redirectURI
}

func BuiltinGeminiClientID() string {
	// Client ID is no longer bundled in source. Return empty; callers must
	// configure the credential via config.yaml.
	return ""
}
