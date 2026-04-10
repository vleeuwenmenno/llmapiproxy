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
type Registry struct {
	mu       sync.RWMutex
	backends map[string]Backend
}

func NewRegistry() *Registry {
	return &Registry{
		backends: make(map[string]Backend),
	}
}

// LoadFromConfig creates backends from config and registers them.
// Only enabled backends are registered for routing.
// Supports backend types: openai, copilot, codex.
func (r *Registry) LoadFromConfig(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends = make(map[string]Backend, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		b, err := r.createBackend(bc)
		if err != nil {
			log.Printf("warning: skipping backend %q: %v", bc.Name, err)
			continue
		}
		r.backends[bc.Name] = b
	}
}

// createBackend instantiates the appropriate backend based on the config type.
func (r *Registry) createBackend(bc config.BackendConfig) (Backend, error) {
	switch bc.Type {
	case "copilot":
		return newCopilotBackendFromConfig(bc)
	case "codex":
		// CodexBackend will be implemented in a subsequent feature.
		// For now, register an OpenAI-compatible placeholder.
		return NewOpenAI(bc), nil
	case "openai", "":
		return NewOpenAI(bc), nil
	default:
		return nil, fmt.Errorf("unknown backend type %q", bc.Type)
	}
}

// newCopilotBackendFromConfig creates a CopilotBackend with its token store,
// discoverer, and exchanger properly wired.
func newCopilotBackendFromConfig(bc config.BackendConfig) (*CopilotBackend, error) {
	// Determine token file path.
	tokenPath := "copilot-token.json"
	if bc.OAuth != nil && bc.OAuth.TokenPath != "" {
		tokenPath = bc.OAuth.TokenPath
	}
	// Use a per-backend token file to support multiple Copilot backends.
	if !strings.Contains(tokenPath, "/") {
		tokenPath = filepath.Join("tokens", bc.Name+"-token.json")
	}

	ts, err := oauth.NewTokenStore(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("creating token store: %w", err)
	}

	discoverer := oauth.NewDiscoverer(oauth.WithTokenStore(ts))
	exchanger := oauth.NewCopilotExchanger(ts)

	return NewCopilotBackend(bc, discoverer, exchanger, ts), nil
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
