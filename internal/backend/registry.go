package backend

import (
	"fmt"
	"strings"
	"sync"

	"github.com/menno/llmapiproxy/internal/config"
)

// Registry maps model prefixes to backends and resolves routing.
type Registry struct {
	mu        sync.RWMutex
	backends  map[string]Backend
	rrTracker *RoundRobinTracker
}

func NewRegistry() *Registry {
	return &Registry{
		backends:  make(map[string]Backend),
		rrTracker: newRoundRobinTracker(),
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

// ResolveRoute returns an ordered list of RouteEntry values for the given model,
// consulting the explicit routing config first and falling back to prefix/wildcard resolution.
// It also returns the resolved routing strategy ("priority", "round-robin", or "race").
func (r *Registry) ResolveRoute(model string, routing config.RoutingConfig) ([]RouteEntry, string, error) {
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
				strategy := mr.Strategy
				if strategy == "" {
					strategy = routing.Strategy
				}
				if strategy == "" {
					strategy = config.StrategyPriority
				}
				if strategy == config.StrategyRoundRobin {
					entries = r.rrTracker.Next(model, entries)
				}
				return entries, strategy, nil
			}
		}
	}

	parts := strings.SplitN(model, "/", 2)
	if len(parts) == 2 {
		if b, ok := r.backends[parts[0]]; ok {
			modelID := parts[1]
			if b.SupportsModel(modelID) {
				return []RouteEntry{{Backend: b, ModelID: modelID}}, config.StrategyPriority, nil
			}
		}
	}

	for _, b := range r.backends {
		if b.SupportsModel(model) {
			return []RouteEntry{{Backend: b, ModelID: model}}, config.StrategyPriority, nil
		}
	}

	return nil, "", fmt.Errorf("no backend found for model %q", model)
}
