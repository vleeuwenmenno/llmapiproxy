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
	"github.com/menno/llmapiproxy/internal/identity"
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
	domain      string                       // externally-reachable domain for OAuth callbacks and links
	rrTracker   *RoundRobinTracker

	// modelCacheStore persists model lists to disk so they survive restarts.
	// Preserved across config reloads.
	modelCacheStore *ModelCacheStore

	// modelIndex is the single source of truth for canonical model identity
	// and cross-backend overlap detection. Built after model caches are warmed.
	// Access via ModelIndex() — may be nil during early startup.
	modelIndex *ModelIndex

	// backendConfigs stores the last loaded backend configs, needed when
	// rebuilding the model index (to resolve backend types).
	backendConfigs []config.BackendConfig
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

	// Store the listen address and domain for deriving OAuth redirect URIs.
	r.listenAddr = cfg.Server.Listen
	r.domain = cfg.Server.Domain

	cacheTTL := cfg.Server.ModelCacheTTL

	// Create or reuse the disk model cache store.
	if r.modelCacheStore == nil {
		cacheDir := filepath.Join("data", "caches")
		store, err := NewModelCacheStore(cacheDir)
		if err != nil {
			log.Warn().Err(err).Msg("failed to create model cache store; disk caching disabled")
		} else {
			r.modelCacheStore = store
			log.Info().Str("path", cacheDir).Msg("model cache store initialized")
		}
	}

	newBackends := make(map[string]Backend, len(cfg.Backends))
	newTokenStores := make(map[string]*oauth.TokenStore)

	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			log.Debug().Str("backend", bc.Name).Msg("skipping disabled backend")
			continue
		}

		var existingTS *oauth.TokenStore
		if bc.IsOAuthBackend() {
			existingTS = r.tokenStores[bc.Name]
		}

		b, ts, err := r.createBackend(bc, existingTS, cacheTTL, cfg)
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

	for name, ts := range r.tokenStores {
		if _, stillExists := newTokenStores[name]; !stillExists {
			if err := ts.Delete(); err != nil {
				log.Warn().Err(err).Str("backend", name).Msg("failed to delete stale token store")
			} else {
				log.Info().Str("backend", name).Msg("deleted stale token store")
			}
		}
	}
	if r.modelCacheStore != nil {
		for name := range r.backends {
			if _, stillExists := newBackends[name]; !stillExists {
				r.modelCacheStore.Invalidate(name)
				log.Info().Str("backend", name).Msg("invalidated stale model cache")
			}
		}
	}

	r.backends = newBackends
	r.tokenStores = newTokenStores
	r.backendConfigs = cfg.Backends

	// Warm model caches in the background so SupportsModel works immediately.
	// The model index is built after warming completes.
	go r.warmModelCaches(cacheTTL)
}

// warmModelCaches calls ListModels on every registered backend to populate
// their model caches. This runs in the background at startup so that
// SupportsModel returns accurate results from the first request.
// After warming, it builds (or rebuilds) the ModelIndex.
func (r *Registry) warmModelCaches(cacheTTL time.Duration) {
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
				log.Warn().Err(err).Str("backend", name).Str("cache_ttl", cacheTTL.String()).Msg("model cache warming failed")
				return
			}
			log.Info().Str("backend", name).Int("models", len(models)).Str("cache_ttl", cacheTTL.String()).Msg("model cache warmed")
		}(name, b)
	}
	wg.Wait()

	// Build the model index now that all caches are populated.
	r.buildModelIndex()
}

// buildModelIndex creates or rebuilds the model index from current backends.
// Called after warmModelCaches completes and from RebuildIndex.
func (r *Registry) buildModelIndex() {
	r.mu.RLock()
	backends := make(map[string]Backend, len(r.backends))
	for k, v := range r.backends {
		backends[k] = v
	}
	cfgs := r.backendConfigs
	r.mu.RUnlock()

	idx := NewModelIndex(DefaultCanonicalizeRules())

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	idx.Build(ctx, backends, cfgs)

	r.mu.Lock()
	r.modelIndex = idx
	r.mu.Unlock()
}

// RebuildIndex forces a rebuild of the model index from current backend data.
// Safe to call from any goroutine. The new index atomically replaces the old one.
func (r *Registry) RebuildIndex() {
	r.buildModelIndex()
}

// ModelIndex returns the current model index, or nil if it hasn't been built yet
// (e.g. during early startup before model caches are warmed).
func (r *Registry) ModelIndex() *ModelIndex {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.modelIndex
}

// FlatModelList returns deduplicated models with routing-aware backend lists.
// This is the single source of truth for "which models exist and who serves them".
// It combines the ModelIndex (for canonical IDs and overlap detection) with
// ResolveRoute (for routing-aware backend ordering).
//
// If the ModelIndex is not yet built (early startup), it falls back to
// iterating all backends and deduplicating manually.
func (r *Registry) FlatModelList(ctx context.Context, routing config.RoutingConfig) []Model {
	idx := r.ModelIndex()
	if idx == nil {
		return r.flatModelListFallback(ctx, routing)
	}

	indexed := idx.FlatModels()
	result := make([]Model, 0, len(indexed))

	for _, im := range indexed {
		m := Model{
			ID:              im.CanonicalID,
			Object:          "model",
			Created:         1774620000,
			OwnedBy:         im.OwnedBy,
			DisplayName:     im.DisplayName,
			ContextLength:   im.ContextLength,
			MaxOutputTokens: im.MaxOutputTokens,
			Capabilities:    im.Capabilities,
		}

		// Determine backend order: prefer explicit routing config, fall back to index order.
		strategy := routing.Strategy
		if strategy == "" {
			strategy = config.StrategyPriority
		}

		routedBackends := make([]string, 0, len(im.Backends))
		for _, ref := range im.Backends {
			routedBackends = append(routedBackends, ref.BackendName)
		}

		entries, resolvedStrategy, _, err := r.ResolveRoute(im.CanonicalID, routing)
		if err == nil && len(entries) > 0 {
			routedBackends = make([]string, 0, len(entries))
			sources := make(map[string]string, len(entries))
			for _, re := range entries {
				routedBackends = append(routedBackends, re.Backend.Name())
				if re.Source != "" {
					sources[re.Backend.Name()] = re.Source
				}
			}
			strategy = resolvedStrategy
			if len(sources) > 0 {
				m.BackendSources = sources
			}
		}

		m.AvailableBackends = routedBackends
		m.RoutingStrategy = strategy

		if len(routedBackends) > 0 {
			m.OwnedBy = routedBackends[0]
		}

		result = append(result, m)
	}

	return result
}

// flatModelListFallback is the pre-ModelIndex dedup logic used during early
// startup before model caches are warmed and the index is built.
func (r *Registry) flatModelListFallback(ctx context.Context, routing config.RoutingConfig) []Model {
	type modelEntry struct {
		model    Model
		backends []string
	}
	seen := make(map[string]*modelEntry)
	var order []string

	for _, b := range r.All() {
		models, err := b.ListModels(ctx)
		if err != nil {
			log.Warn().Err(err).Str("backend", b.Name()).Msg("flatModelList fallback: error listing models")
			continue
		}
		for _, m := range models {
			if m.Disabled {
				continue
			}
			baseID := strings.TrimPrefix(m.ID, b.Name()+"/")

			if existing, ok := seen[baseID]; ok {
				if m.ContextLength != nil && (existing.model.ContextLength == nil || *m.ContextLength > *existing.model.ContextLength) {
					existing.model.ContextLength = m.ContextLength
				}
				if m.MaxOutputTokens != nil && (existing.model.MaxOutputTokens == nil || *m.MaxOutputTokens > *existing.model.MaxOutputTokens) {
					existing.model.MaxOutputTokens = m.MaxOutputTokens
				}
				capSet := make(map[string]bool, len(existing.model.Capabilities))
				for _, c := range existing.model.Capabilities {
					capSet[c] = true
				}
				for _, c := range m.Capabilities {
					if !capSet[c] {
						existing.model.Capabilities = append(existing.model.Capabilities, c)
					}
				}
				existing.backends = append(existing.backends, b.Name())
			} else {
				mCopy := m
				mCopy.ID = baseID
				if mCopy.OwnedBy == "" {
					mCopy.OwnedBy = b.Name()
				}
				seen[baseID] = &modelEntry{
					model:    mCopy,
					backends: []string{b.Name()},
				}
				order = append(order, baseID)
			}
		}
	}

	result := make([]Model, 0, len(order))
	for _, id := range order {
		entry := seen[id]
		m := entry.model

		strategy := routing.Strategy
		if strategy == "" {
			strategy = config.StrategyPriority
		}

		routedBackends := entry.backends
		entries, resolvedStrategy, _, err := r.ResolveRoute(id, routing)
		if err == nil && len(entries) > 0 {
			routedBackends = make([]string, 0, len(entries))
			for _, re := range entries {
				routedBackends = append(routedBackends, re.Backend.Name())
			}
			strategy = resolvedStrategy
		}

		m.AvailableBackends = routedBackends
		m.RoutingStrategy = strategy

		if len(routedBackends) > 1 {
			m.OwnedBy = routedBackends[0]
		}

		result = append(result, m)
	}

	return result
}

// createBackend instantiates the appropriate backend based on the config type.
// If existingTS is non-nil, it is reused instead of creating a new token store.
// Returns the backend and (for OAuth backends) the token store used.
func (r *Registry) createBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, cacheTTL time.Duration, cfg *config.Config) (Backend, *oauth.TokenStore, error) {
	// Resolve identity profile: backend-specific > global > none.
	profileID := bc.IdentityProfile
	if profileID == "" {
		profileID = cfg.IdentityProfile
	}
	if profileID == "" && bc.Type == "gemini" {
		profileID = "gemini-cli"
	}
	var customProfiles []identity.Profile
	for _, cp := range cfg.CustomIdentityProfiles {
		customProfiles = append(customProfiles, identity.Profile{
			ID:          cp.ID,
			DisplayName: cp.DisplayName,
			UserAgent:   cp.UserAgent,
			Headers:     cp.Headers,
		})
	}
	profile := identity.Resolve(profileID, customProfiles)

	switch bc.Type {
	case "copilot":
		b, ts, err := r.createCopilotBackend(bc, existingTS, profile)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "codex":
		b, ts, err := r.createCodexBackend(bc, existingTS, cacheTTL, profile)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "gemini":
		b, ts, err := r.createGeminiBackend(bc, existingTS, cacheTTL, profile)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "anthropic":
		b := NewAnthropic(bc, cacheTTL, profile)
		b.SetModelCacheStore(r.modelCacheStore)
		return b, nil, nil
	case "ollama":
		b := NewOllama(bc, cacheTTL, profile)
		b.SetModelCacheStore(r.modelCacheStore)
		return b, nil, nil
	case "openai", "":
		b := NewOpenAI(bc, cacheTTL, profile)
		b.SetModelCacheStore(r.modelCacheStore)
		return b, nil, nil
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
		tokenPath = filepath.Join("data", "tokens", bc.Name+"-token.json")
	}
	return tokenPath
}

// createCopilotBackend creates a CopilotBackend with device code flow support,
// reusing the existing token store if provided.
func (r *Registry) createCopilotBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, profile *identity.Profile) (*CopilotBackend, *oauth.TokenStore, error) {
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

	return NewCopilotBackend(bc, deviceCodeHandler, ts, r.modelCacheStore, profile), ts, nil
}

// createCodexBackend creates a CodexBackend, reusing the existing token store
// if provided. The OAuth redirect URI is derived from the server's listen
// address and the backend name. A device code handler is also created to
// support device code flow as an alternative login method.
func (r *Registry) createCodexBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, cacheTTL time.Duration, profile *identity.Profile) (*CodexBackend, *oauth.TokenStore, error) {
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
	// localhost:1455. Custom OAuth clients keep using the proxy-hosted
	// callback route under /ui/oauth/callback/<backend>.
	if !usesBuiltinCodexClient {
		oauthCfg.RedirectURI = oauth.DeriveRedirectURI(r.domain, r.listenAddr, bc.Name)
	}

	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	// Create a device code handler for headless/SSH login support.
	deviceCodeHandler := oauth.NewCodexDeviceCodeHandler(ts, oauthCfg)

	return NewCodexBackend(bc, oauthHandler, ts, deviceCodeHandler, cacheTTL, r.modelCacheStore, profile), ts, nil
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

func (r *Registry) createGeminiBackend(bc config.BackendConfig, existingTS *oauth.TokenStore, cacheTTL time.Duration, profile *identity.Profile) (*GeminiBackend, *oauth.TokenStore, error) {
	tokenPath := bc.OAuth.TokenPath
	if tokenPath == "" {
		tokenPath = filepath.Join("data", "tokens", bc.Name+"-token.json")
	}

	var ts *oauth.TokenStore
	if existingTS != nil {
		ts = existingTS
	} else {
		var err error
		ts, err = oauth.NewTokenStore(tokenPath)
		if err != nil {
			return nil, nil, fmt.Errorf("creating Gemini token store: %w", err)
		}
	}

	oauthCfg := oauth.DefaultGeminiOAuthConfig()
	if bc.OAuth.ClientID != "" {
		oauthCfg.ClientID = bc.OAuth.ClientID
	}
	if bc.OAuth.ClientSecret != "" {
		oauthCfg.ClientSecret = bc.OAuth.ClientSecret
	}
	if bc.OAuth.RedirectURI != "" {
		oauthCfg.RedirectURI = bc.OAuth.RedirectURI
	}

	oauthHandler := oauth.NewGeminiOAuthHandler(ts, oauthCfg)

	return NewGeminiBackend(bc, oauthHandler, ts, cacheTTL, r.modelCacheStore, profile), ts, nil
}

func (r *Registry) HandleGeminiLoopbackCallback(ctx context.Context, code string, state string) (string, error) {
	r.mu.RLock()
	candidates := make([]*GeminiBackend, 0, len(r.backends))
	for _, b := range r.backends {
		geminiBackend, ok := b.(*GeminiBackend)
		if ok {
			candidates = append(candidates, geminiBackend)
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
		// Reset onboarding state so the next request re-fetches the
		// cloudAIProject from loadCodeAssist with the fresh token.
		candidate.ResetOnboarding()
		return candidate.Name(), nil
	}

	return "", fmt.Errorf("no pending gemini oauth flow matched the callback state")
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
					rawID := model
					if r.modelIndex != nil {
						if resolved, ok2 := r.modelIndex.ResolveBackendModelID(model, bName); ok2 {
							rawID = resolved
						} else {
							rawID = b.ResolveModelID(model)
						}
					} else {
						rawID = b.ResolveModelID(model)
					}
					entries = append(entries, RouteEntry{Backend: b, ModelID: rawID, Source: "config"})
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

	// Fallback: use the ModelIndex to find ALL backends serving this model.
	// This replaces the old single-backend map iteration that was nondeterministic.
	if idx := r.modelIndex; idx != nil {
		refs := idx.BackendsFor(model)
		if len(refs) > 0 {
			strategy := routing.Strategy
			if strategy == "" {
				strategy = config.StrategyPriority
			}
			staggerDelayMs := routing.StaggerDelayMs

			var entries []RouteEntry
			for _, ref := range refs {
				if b, ok := r.backends[ref.BackendName]; ok {
					entries = append(entries, RouteEntry{
						Backend: b,
						ModelID: ref.RawModelID,
						Source:  "discovered",
					})
				}
			}
			if len(entries) > 0 {
				if strategy == config.StrategyRoundRobin {
					entries = r.rrTracker.Next(model, entries)
				}
				return entries, strategy, staggerDelayMs, nil
			}
		}
	}

	// Last resort: iterate backends directly (handles nil index during startup).
	// Collect ALL matching backends, sorted by name for deterministic ordering.
	type matchEntry struct {
		name    string
		backend Backend
	}
	var matches []matchEntry
	for name, b := range r.backends {
		if b.SupportsModel(model) {
			matches = append(matches, matchEntry{name: name, backend: b})
		}
	}
	if len(matches) > 0 {
		sort.Slice(matches, func(i, j int) bool {
			return matches[i].name < matches[j].name
		})
		strategy := routing.Strategy
		if strategy == "" {
			strategy = config.StrategyPriority
		}
		entries := make([]RouteEntry, 0, len(matches))
		for _, m := range matches {
			entries = append(entries, RouteEntry{
				Backend: m.backend,
				ModelID: m.backend.ResolveModelID(model),
				Source:  "discovered",
			})
		}
		if strategy == config.StrategyRoundRobin {
			entries = r.rrTracker.Next(model, entries)
		}
		return entries, strategy, routing.StaggerDelayMs, nil
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
		case *OllamaBackend:
			tb.ClearModelCache()
		}
	}
	log.Info().Msg("cleared model caches for all backends")
}

// CleanupBackend removes persisted tokens and model caches for a backend
// that has been removed from the configuration.
func (r *Registry) CleanupBackend(name string) {
	r.mu.Lock()
	ts := r.tokenStores[name]
	delete(r.tokenStores, name)
	r.mu.Unlock()

	if ts != nil {
		if err := ts.Delete(); err != nil {
			log.Warn().Err(err).Str("backend", name).Msg("failed to delete token store")
		} else {
			log.Info().Str("backend", name).Msg("deleted token store")
		}
	}

	if r.modelCacheStore != nil {
		r.modelCacheStore.Invalidate(name)
		log.Info().Str("backend", name).Msg("invalidated model cache")
	}
}
