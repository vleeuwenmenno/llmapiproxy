package backend

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"

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
}

func NewRegistry() *Registry {
	return &Registry{
		backends:    make(map[string]Backend),
		tokenStores: make(map[string]*oauth.TokenStore),
	}
}

// LoadFromConfig creates backends from config and registers them.
// Only enabled backends are registered for routing.
// Supports backend types: openai, copilot, codex.
// Token stores for OAuth backends are preserved across reloads — if a backend
// with the same name exists in the old set, its token store is reused.
func (r *Registry) LoadFromConfig(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Store the listen address for deriving OAuth redirect URIs.
	r.listenAddr = cfg.Server.Listen

	newBackends := make(map[string]Backend, len(cfg.Backends))
	newTokenStores := make(map[string]*oauth.TokenStore)

	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}

		// For OAuth backends, try to reuse the existing token store.
		var existingTS *oauth.TokenStore
		if bc.IsOAuthBackend() {
			existingTS = r.tokenStores[bc.Name]
		}

		b, ts, err := r.createBackend(bc, existingTS)
		if err != nil {
			log.Printf("warning: skipping backend %q: %v", bc.Name, err)
			continue
		}
		newBackends[bc.Name] = b
		if ts != nil {
			newTokenStores[bc.Name] = ts
		}
	}

	r.backends = newBackends
	r.tokenStores = newTokenStores
}

// createBackend instantiates the appropriate backend based on the config type.
// If existingTS is non-nil, it is reused instead of creating a new token store.
// Returns the backend and (for OAuth backends) the token store used.
func (r *Registry) createBackend(bc config.BackendConfig, existingTS *oauth.TokenStore) (Backend, *oauth.TokenStore, error) {
	switch bc.Type {
	case "copilot":
		b, ts, err := r.createCopilotBackend(bc, existingTS)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "codex":
		b, ts, err := r.createCodexBackend(bc, existingTS)
		if err != nil {
			return nil, nil, err
		}
		return b, ts, nil
	case "openai", "":
		return NewOpenAI(bc), nil, nil
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
// address and the backend name.
func (r *Registry) createCodexBackend(bc config.BackendConfig, existingTS *oauth.TokenStore) (*CodexBackend, *oauth.TokenStore, error) {
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
	if bc.OAuth != nil {
		if bc.OAuth.ClientID != "" {
			oauthCfg.ClientID = bc.OAuth.ClientID
		}
		if bc.OAuth.AuthURL != "" {
			oauthCfg.AuthURL = bc.OAuth.AuthURL
		}
		if bc.OAuth.TokenURL != "" {
			oauthCfg.TokenURL = bc.OAuth.TokenURL
		}
		if len(bc.OAuth.Scopes) > 0 {
			oauthCfg.Scope = strings.Join(bc.OAuth.Scopes, " ")
		}
	}

	// Derive the redirect URI from the server's actual listen address and
	// the backend name, instead of using the hardcoded default.
	redirectURI := oauth.DeriveRedirectURI(r.listenAddr, bc.Name)
	oauthCfg.RedirectURI = redirectURI

	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	return NewCodexBackend(bc, oauthHandler, ts), ts, nil
}

// Resolve parses a model string like "openrouter/openai/gpt-5.2" and returns
// the matching backend and the model ID to forward (e.g., "openai/gpt-5.2").
func (r *Registry) Resolve(model string) (Backend, string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		if b, ok := r.backends[parts[0]]; ok {
			modelID := parts[1]
			if b.SupportsModel(modelID) {
				return b, modelID, nil
			}
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
// that implement OAuthStatusProvider.
func (r *Registry) OAuthStatuses() []OAuthStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var statuses []OAuthStatus
	for _, b := range r.backends {
		if sp, ok := b.(OAuthStatusProvider); ok {
			statuses = append(statuses, sp.OAuthStatus())
		}
	}
	return statuses
}

// GetTokenStore returns the token store for the named backend, or nil.
func (r *Registry) GetTokenStore(name string) *oauth.TokenStore {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tokenStores[name]
}

// ResolveRoute returns an ordered list of RouteEntry values for the given model,
// consulting the explicit routing config first and falling back to prefix/wildcard resolution.
func (r *Registry) ResolveRoute(model string, routing config.RoutingConfig) ([]RouteEntry, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, mr := range routing.Models {
		if mr.Model == model {
			var entries []RouteEntry
			for _, bName := range mr.Backends {
				if b, ok := r.backends[bName]; ok {
					entries = append(entries, RouteEntry{Backend: b, ModelID: model})
				}
			}
			if len(entries) > 0 {
				return entries, nil
			}
		}
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		if b, ok := r.backends[parts[0]]; ok {
			modelID := parts[1]
			if b.SupportsModel(modelID) {
				return []RouteEntry{{Backend: b, ModelID: modelID}}, nil
			}
		}
	}

	for _, b := range r.backends {
		if b.SupportsModel(model) {
			return []RouteEntry{{Backend: b, ModelID: model}}, nil
		}
	}

	return nil, fmt.Errorf("no backend found for model %q", model)
}
