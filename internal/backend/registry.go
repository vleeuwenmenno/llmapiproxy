package backend

import (
	"fmt"
	"strings"
	"sync"

	"github.com/menno/llmapiproxy/internal/config"
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
func (r *Registry) LoadFromConfig(cfg *config.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.backends = make(map[string]Backend, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		r.backends[bc.Name] = NewOpenAI(bc)
	}
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
