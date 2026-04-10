package backend

import (
	"testing"

	"github.com/menno/llmapiproxy/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// --- Registry.LoadFromConfig type switch tests ---

func TestRegistry_LoadFromConfig_OpenAI(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if !r.Has("openrouter") {
		t.Error("openrouter backend not registered")
	}
	b := r.Get("openrouter")
	if b == nil {
		t.Fatal("openrouter backend is nil")
	}
	if b.Name() != "openrouter" {
		t.Errorf("Name() = %q, want %q", b.Name(), "openrouter")
	}
}

func TestRegistry_LoadFromConfig_Copilot(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if !r.Has("copilot") {
		t.Error("copilot backend not registered")
	}
	b := r.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend is nil")
	}
	if b.Name() != "copilot" {
		t.Errorf("Name() = %q, want %q", b.Name(), "copilot")
	}
}

func TestRegistry_LoadFromConfig_Codex(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if !r.Has("codex") {
		t.Error("codex backend not registered")
	}
	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}
	if b.Name() != "codex" {
		t.Errorf("Name() = %q, want %q", b.Name(), "codex")
	}
}

func TestRegistry_LoadFromConfig_UnknownTypeSkipped(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "unknown",
				Type:    "unknown_type",
				BaseURL: "https://example.com",
				APIKey:  "key",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if r.Has("unknown") {
		t.Error("unknown backend type should be skipped")
	}
}

func TestRegistry_LoadFromConfig_DisabledSkipped(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "disabled",
				Type:    "openai",
				BaseURL: "https://example.com",
				APIKey:  "key",
				Enabled: boolPtr(false),
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if r.Has("disabled") {
		t.Error("disabled backend should not be registered")
	}
}

func TestRegistry_LoadFromConfig_MixedTypes(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if len(r.Names()) != 3 {
		t.Errorf("expected 3 backends, got %d", len(r.Names()))
	}
	for _, name := range []string{"openrouter", "copilot", "codex"} {
		if !r.Has(name) {
			t.Errorf("backend %q not registered", name)
		}
	}
}

// --- Hot-reload tests ---

func TestRegistry_HotReload_AddsBackend(t *testing.T) {
	r := NewRegistry()

	// Initial config with one backend.
	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
		},
	}
	r.LoadFromConfig(cfg1)

	if !r.Has("openrouter") {
		t.Error("openrouter not registered")
	}
	if r.Has("copilot") {
		t.Error("copilot should not be registered yet")
	}

	// Reload with copilot added.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}
	r.LoadFromConfig(cfg2)

	if !r.Has("openrouter") {
		t.Error("openrouter should still be registered")
	}
	if !r.Has("copilot") {
		t.Error("copilot should be registered after reload")
	}
}

func TestRegistry_HotReload_RemovesBackend(t *testing.T) {
	r := NewRegistry()

	// Initial config with two backends.
	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}
	r.LoadFromConfig(cfg1)

	if !r.Has("copilot") {
		t.Error("copilot should be registered initially")
	}

	// Reload with copilot removed.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
		},
	}
	r.LoadFromConfig(cfg2)

	if !r.Has("openrouter") {
		t.Error("openrouter should still be registered")
	}
	if r.Has("copilot") {
		t.Error("copilot should be removed after reload")
	}
}

func TestRegistry_HotReload_UpdatesBackend(t *testing.T) {
	r := NewRegistry()

	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				Models:  []string{"gpt-4o"},
			},
		},
	}
	r.LoadFromConfig(cfg1)

	b := r.Get("copilot")
	if !b.SupportsModel("gpt-4o") {
		t.Error("copilot should support gpt-4o")
	}

	// Reload with different models.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				Models:  []string{"o3"},
			},
		},
	}
	r.LoadFromConfig(cfg2)

	b = r.Get("copilot")
	if b.SupportsModel("gpt-4o") {
		t.Error("copilot should no longer support gpt-4o after config update")
	}
	if !b.SupportsModel("o3") {
		t.Error("copilot should support o3 after config update")
	}
}

func TestRegistry_HotReload_MultipleIndependent(t *testing.T) {
	r := NewRegistry()

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg)

	if !r.Has("copilot") || !r.Has("codex") {
		t.Error("both OAuth backends should be registered")
	}

	// Verify they are independent — removing one doesn't affect the other.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg2)

	if r.Has("copilot") {
		t.Error("copilot should be removed")
	}
	if !r.Has("codex") {
		t.Error("codex should still be registered")
	}
}

// --- Helper method tests ---

func TestRegistry_Names(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	names := r.Names()
	if len(names) != 2 {
		t.Fatalf("expected 2 names, got %d", len(names))
	}

	namesMap := map[string]bool{}
	for _, n := range names {
		namesMap[n] = true
	}
	if !namesMap["openrouter"] || !namesMap["copilot"] {
		t.Errorf("Names() = %v, want [openrouter, copilot]", names)
	}
}

func TestRegistry_Get_NonExistent(t *testing.T) {
	r := NewRegistry()
	if b := r.Get("nonexistent"); b != nil {
		t.Error("Get should return nil for nonexistent backend")
	}
}

func TestRegistry_Has_NonExistent(t *testing.T) {
	r := NewRegistry()
	if r.Has("nonexistent") {
		t.Error("Has should return false for nonexistent backend")
	}
}

func TestRegistry_All_Empty(t *testing.T) {
	r := NewRegistry()
	all := r.All()
	if len(all) != 0 {
		t.Errorf("All() on empty registry should return empty slice, got %d", len(all))
	}
}
