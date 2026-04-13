package backend

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
)

// Registry maps model prefixes to backends and resolves routing.
// It preserves token stores across config hot-reloads so that in-memory
// tokens are not lost when backends are re-created.
type Registry struct {
	mu          sync.RWMutex
	backends    map[string]Backend
	tokenStores map[string]*oauth.TokenStore // preserved across reloads
	listenAddr  string                       // server listen address for deriving OAuth redirect URIs
	rrTracker   *RoundRobinTracker
}

func NewRegistry() *Registry {
	return &Registry{
		backends:    make(map[string]Backend),
		tokenStores: make(map[string]*oauth.TokenStore),
		rrTracker:   newRoundRobinTracker(),
	}
}

// LoadFromConfig creates backends from config and registers them.
// Only enabled backends are registered for routing.
// Supports backend types: openai, anthropic, copilot, codex.
// Token stores for OAuth backends are preserved across reloads — if a backend
// with the same name exists in the old set, its token store is reused.
func (r *Registry) LoadFromConfig(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Store the listen address for deriving OAuth redirect URIs.
	r.listenAddr = cfg.Server.Listen

	cacheTTL := cfg.Server.ModelCacheTTL

	newBackends := make(map[string]Backend, len(cfg.Backends))
	newTokenStores := make(map[string]*oauth.TokenStore)

	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			log.Debug().Str("backend", bc.Name).Msg("skipping disabled backend")
			continue
		}

		// For OAuth backends, try to reuse the existing token store.
		var existingTS *oauth.TokenStore
		if bc.IsOAuthBackend() {
			existingTS = r.tokenStores[bc.Name]
		}

		b, ts, err := r.createBackend(bc, existingTS, cacheTTL)
		if err != nil {
			log.Warn().Str("backend", bc.Name).Err(err).Msg("skipping backend")
			continue
		}
		newBackends[bc.Name] = b
		if ts != nil {
			newTokenStores[bc.Name] = ts
		}
		log.Info().Str("backend", bc.Name).Msg("registered backend")
	}

	r.backends = newBackends
	r.tokenStores = newTokenStores

	// Warm model caches in the background so SupportsModel works immediately.
	go r.warmModelCaches()
}

// warmModelCaches calls ListModels on every registered backend to populate
// their model caches. This runs in the background at startup so that
// SupportsModel returns accurate results from the first request.
func (r *Registry) warmModelCaches() {
	r.mu.RLock()
	backends := make(map[string]Backend, len(r.backends))
	for k, v := range r.backends {
		backends[k] = v
	}
	r.mu.RUnlock()

	const warmTimeout = 15 * time.Second

	var wg sync.WaitGroup
	for name, b := range backends {
		wg.Add(1)
		go func(name string, b Backend) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), warmTimeout)
			defer cancel()
			models, err := b.ListModels(ctx)
			if err != nil {
				log.Warn().Err(err).Str("backend", name).Msg("model cache warming failed")
				return
			}
			log.Info().Str("backend", name).Int("models", len(models)).Msg("model cache warmed")
		}(name, b)
	}
	wg.Wait()
	log.Info().Msg("model cache warming complete")
}

// createBackend instantiates the appropriate backend based on the config type.
// If existingTS is non-nil, it is reused instead of creating a new token store.
// Returns the backend and (for OAuth backends) the token store used.
func (r *Registry) createBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, cacheTTL time.Duration) (Backend, *oauth.TokenStore, error) {
	switch bc.Type {
	case "copilot":
		b, ts, err := r.createCopilotBackend(bc, existingTS)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "codex":
		b, ts, err := r.createCodexBackend(bc, existingTS, cacheTTL)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "anthropic":
		return NewAnthropic(bc, cacheTTL), nil, nil
	case "openai", "":
		return NewOpenAI(bc, cacheTTL), nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown backend type %q", bc.Type)
	}
}

// tokenStorePath determines the token file path for a backend.
func tokenStorePath(bc config.BackendConfig) string {
	tokenPath := bc.Type + "-token.json"
	if bc.OAuth != nil && bc.OAuth.TokenPath != "" {
		tokenPath = bc.OAuth.TokenPath
	}
	if !strings.Contains(tokenPath, "/") {
		tokenPath = filepath.Join("tokens", bc.Name+"-token.json")
	}
	return tokenPath
}

// createCopilotBackend creates a CopilotBackend with device code flow support,
// reusing the existing token store if provided.
func (r *Registry) createCopilotBackend(bc config.BackendConfig, existingTS *oauth.TokenStore) (*CopilotBackend, *oauth.TokenStore, error) {
	tokenPath := tokenStorePath(bc)

	ts := existingTS
	var err error
	if ts == nil {
		ts, err = oauth.NewTokenStore(tokenPath)
		if err != nil {
			return nil, nil, fmt.Errorf("creating token store: %w", err)
		}
	}

	// Build device code handler with optional config overrides.
	var deviceCodeOpts []oauth.DeviceCodeHandlerOption
	if bc.OAuth != nil {
		if bc.OAuth.ClientID != "" {
			deviceCodeOpts = append(deviceCodeOpts, oauth.WithDeviceCodeClientID(bc.OAuth.ClientID))
		}
	}
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts, deviceCodeOpts...)

	return NewCopilotBackend(bc, deviceCodeHandler, ts), ts, nil
}

// createCodexBackend creates a CodexBackend, reusing the existing token store
// if provided. The OAuth redirect URI is derived from the server's listen
// address and the backend name. A device code handler is also created to
// support device code flow as an alternative login method.
func (r *Registry) createCodexBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, cacheTTL time.Duration) (*CodexBackend, *oauth.TokenStore, error) {
	tokenPath := tokenStorePath(bc)

	ts := existingTS
	var err error
	if ts == nil {
		ts, err = oauth.NewTokenStore(tokenPath)
		if err != nil {
			return nil, nil, fmt.Errorf("creating token store: %w", err)
		}
	}

	// Build OAuth config from the backend config, falling back to defaults.
	oauthCfg := oauth.DefaultCodexOAuthConfig()
	usesBuiltinCodexClient := true
	if bc.OAuth != nil {
		if clientID := normalizeCodexClientID(bc.OAuth.ClientID); clientID != "" {
			oauthCfg.ClientID = clientID
		}
		if authURL := normalizeCodexAuthURL(bc.OAuth.AuthURL); authURL != "" {
			oauthCfg.AuthURL = authURL
		}
		if bc.OAuth.TokenURL != "" {
			oauthCfg.TokenURL = bc.OAuth.TokenURL
		}
		if scopes := normalizeCodexScopes(bc.OAuth.Scopes); len(scopes) > 0 {
			oauthCfg.Scope = strings.Join(scopes, " ")
		}
	}
	usesBuiltinCodexClient = oauthCfg.ClientID == oauth.BuiltinCodexClientID()

	// The bundled Codex client expects the official loopback callback on
	// localhost:1455. Custom OAuth clients can keep using the proxy-hosted
	// callback route under /ui/oauth/callback/<backend>.
	if !usesBuiltinCodexClient {
		oauthCfg.RedirectURI = oauth.DeriveRedirectURI(r.listenAddr, bc.Name)
	}

	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	// Create a device code handler for headless/SSH login support.
	deviceCodeHandler := oauth.NewCodexDeviceCodeHandler(ts, oauthCfg)

	return NewCodexBackend(bc, oauthHandler, ts, deviceCodeHandler, cacheTTL), ts, nil
}

// HandleCodexLoopbackCallback routes a loopback OAuth callback to the Codex
// backend that owns the pending state.
func (r *Registry) HandleCodexLoopbackCallback(ctx context.Context, code string, state string) (string, error) {
	r.mu.RLock()
	candidates := make([]*CodexBackend, 0, len(r.backends))
	for _, b := range r.backends {
		codexBackend, ok := b.(*CodexBackend)
		if ok {
			candidates = append(candidates, codexBackend)
		}
	}
	r.mu.RUnlock()

	for _, candidate := range candidates {
		if candidate.GetOAuthHandler().GetPendingState(state) == nil {
			continue
		}
		if err := candidate.HandleCallback(ctx, code, state); err != nil {
			return candidate.Name(), err
		}
		return candidate.Name(), nil
	}

	return "", fmt.Errorf("no pending codex oauth flow matched the callback state")
}

func normalizeCodexClientID(clientID string) string {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return ""
	}

	switch strings.ToLower(clientID) {
	case "your-codex-client-id", "your-client-id", "<your-codex-client-id>", "replace-me":
		log.Warn().Str("client_id", clientID).Msg("ignoring placeholder codex oauth client_id; using built-in client id")
		return ""
	default:
		return clientID
	}
}

func normalizeCodexAuthURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	if strings.EqualFold(u.Host, "auth.openai.com") && u.Path == "/authorize" {
		u.Path = "/oauth/authorize"
		log.Warn().Str("from", raw).Str("to", u.String()).Msg("normalizing legacy Codex auth_url")
		return u.String()
	}

	return raw
}

func normalizeCodexScopes(scopes []string) []string {
	if len(scopes) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(scopes)+1)
	out := make([]string, 0, len(scopes)+1)
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || seen[scope] {
			continue
		}
		seen[scope] = true
		out = append(out, scope)
	}

	if !seen["offline_access"] {
		out = append(out, "offline_access")
	}

	return out
}

// Resolve parses a model string like "openrouter/openai/gpt-5.2" and returns
// the matching backend and the model ID to forward (e.g., "openai/gpt-5.2").
func (r *Registry) Resolve(model string) (Backend, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		if b, ok := r.backends[parts[0]]; ok {
			// Explicit prefix routing — user intentionally targets this backend.
			return b, parts[1], nil
		}
	}

	for _, b := range r.backends {
		if b.SupportsModel(model) {
			return b, model, nil
		}
	}

	return nil, "", fmt.Errorf("no backend found for model %q", model)
}

// All returns all registered backends.
func (r *Registry) All() []Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Backend, 0, len(r.backends))
	for _, b := range r.backends {
		result = append(result, b)
	}
	return result
}

// Get returns a backend by name, or nil if not found.
func (r *Registry) Get(name string) Backend {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.backends[name]
}

// Has returns true if a backend with the given name is registered.
func (r *Registry) Has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.backends[name]
	return ok
}

// Names returns the names of all registered backends.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	return names
}

// OAuthStatuses returns the OAuth authentication status for all backends
// that implement OAuthStatusProvider. The result is sorted alphabetically
// by BackendName for deterministic ordering.
func (r *Registry) OAuthStatuses() []OAuthStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var statuses []OAuthStatus
	for _, b := range r.backends {
		if sp, ok := b.(OAuthStatusProvider); ok {
			statuses = append(statuses, sp.OAuthStatus())
		}
	}
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].BackendName < statuses[j].BackendName
	})
	return statuses
}

// OAuthStatus returns the OAuth authentication status for one named backend.
// The boolean result is false when the backend does not exist or does not
// expose OAuth status.
func (r *Registry) OAuthStatus(name string) (OAuthStatus, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	b, ok := r.backends[name]
	if !ok {
		return OAuthStatus{}, false
	}
	sp, ok := b.(OAuthStatusProvider)
	if !ok {
		return OAuthStatus{}, false
	}
	return sp.OAuthStatus(), true
}

// GetTokenStore returns the token store for the named backend, or nil.
func (r *Registry) GetTokenStore(name string) *oauth.TokenStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tokenStores[name]
}

// ResolveRoute returns an ordered list of RouteEntry values for the given model,
// consulting the explicit routing config first and falling back to prefix/wildcard resolution.
// It also returns the resolved routing strategy and the stagger delay in milliseconds
// (only relevant for the "staggered-race" strategy; 0 means use the default of 500ms).
func (r *Registry) ResolveRoute(model string, routing config.RoutingConfig) ([]RouteEntry, string, int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, mr := range routing.Models {
		if mr.Model == model {
			// Build per-model disabled set.
			skip := make(map[string]bool, len(mr.DisabledBackends))
			for _, bName := range mr.DisabledBackends {
				skip[bName] = true
			}

			var entries []RouteEntry
			for _, bName := range mr.Backends {
				if skip[bName] {
					continue
				}
				if b, ok := r.backends[bName]; ok {
					entries = append(entries, RouteEntry{Backend: b, ModelID: model})
				}
			}
			if len(entries) > 0 {
				strategy := mr.Strategy
				if strategy == "" {
					strategy = routing.Strategy
				}
				if strategy == "" {
					strategy = config.StrategyPriority
				}
				staggerDelayMs := mr.StaggerDelayMs
				if staggerDelayMs == 0 {
					staggerDelayMs = routing.StaggerDelayMs
				}
				if strategy == config.StrategyRoundRobin {
					entries = r.rrTracker.Next(model, entries)
				}
				return entries, strategy, staggerDelayMs, nil
			}
		}
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		if b, ok := r.backends[parts[0]]; ok {
			// Explicit prefix routing — user intentionally targets this backend.
			return []RouteEntry{{Backend: b, ModelID: parts[1]}}, config.StrategyPriority, 0, nil
		}
	}

	for _, b := range r.backends {
		if b.SupportsModel(model) {
			return []RouteEntry{{Backend: b, ModelID: model}}, config.StrategyPriority, 0, nil
		}
	}

	return nil, "", 0, fmt.Errorf("no backend found for model %q", model)
}

// RegisterBackend registers a backend by name, replacing any existing backend
// with the same name. This is primarily used for testing with mock backends.
func (r *Registry) RegisterBackend(name string, b Backend) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.backends[name] = b
}

// ClearAllModelCaches clears the model cache for all registered backends.
func (r *Registry) ClearAllModelCaches() {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, b := range r.backends {
		switch tb := b.(type) {
		case *OpenAIBackend:
			tb.ClearModelCache()
		case *AnthropicBackend:
			tb.ClearModelCache()
		case *CodexBackend:
			tb.ClearModelCache()
		case *CopilotBackend:
			tb.ClearModelCache()
		}
	}
	log.Info().Msg("cleared model caches for all backends")
}
