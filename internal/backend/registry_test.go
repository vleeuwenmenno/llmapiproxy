package backend

import (
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
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

// --- Token preservation tests ---

func TestRegistry_HotReload_PreservesCodexTokens(t *testing.T) {
	r := NewRegistry()

	// Initial config with codex backend.
	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg1)

	// Clean up any stale token files from previous test runs.
	ts := r.GetTokenStore("codex")
	ts.Clear()

	// Get the token store and save a token.
	if ts == nil {
		t.Fatal("token store should exist for codex backend")
	}

	testToken := &oauth.TokenData{
		AccessToken:  "test-access-token-123",
		RefreshToken: "test-refresh-token-456",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	}
	if err := ts.Save(testToken); err != nil {
		t.Fatalf("failed to save token: %v", err)
	}

	// Verify the token is accessible.
	loaded := ts.Get()
	if loaded == nil || loaded.AccessToken != "test-access-token-123" {
		t.Fatal("token should be accessible before reload")
	}

	// Reload config (simulating SIGHUP).
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

	// Verify the token store was preserved.
	ts2 := r.GetTokenStore("codex")
	if ts2 == nil {
		t.Fatal("token store should still exist after reload")
	}

	// The token should still be accessible (same token store instance).
	loaded2 := ts2.Get()
	if loaded2 == nil {
		t.Fatal("token should be preserved across reload")
	}
	if loaded2.AccessToken != "test-access-token-123" {
		t.Errorf("access token mismatch after reload: got %q, want %q", loaded2.AccessToken, "test-access-token-123")
	}
	if loaded2.RefreshToken != "test-refresh-token-456" {
		t.Errorf("refresh token mismatch after reload: got %q, want %q", loaded2.RefreshToken, "test-refresh-token-456")
	}
}

func TestRegistry_HotReload_PreservesCopilotTokens(t *testing.T) {
	r := NewRegistry()

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}
	r.LoadFromConfig(cfg)

	// Save a token.
	ts := r.GetTokenStore("copilot")
	if ts == nil {
		t.Fatal("token store should exist for copilot backend")
	}

	testToken := &oauth.TokenData{
		AccessToken: "test-copilot-token-789",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:      "env:COPILOT_GITHUB_TOKEN",
	}
	if err := ts.Save(testToken); err != nil {
		t.Fatalf("failed to save token: %v", err)
	}

	// Reload.
	r.LoadFromConfig(cfg)

	// Token should be preserved.
	ts2 := r.GetTokenStore("copilot")
	loaded := ts2.Get()
	if loaded == nil {
		t.Fatal("copilot token should be preserved across reload")
	}
	if loaded.AccessToken != "test-copilot-token-789" {
		t.Errorf("copilot token mismatch: got %q, want %q", loaded.AccessToken, "test-copilot-token-789")
	}
}

func TestRegistry_HotReload_RemovesTokenStoreForRemovedBackend(t *testing.T) {
	r := NewRegistry()

	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg1)

	if r.GetTokenStore("codex") == nil {
		t.Fatal("codex token store should exist")
	}

	// Reload without codex.
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

	if r.GetTokenStore("codex") != nil {
		t.Error("codex token store should be cleaned up after removal")
	}
}

// --- OAuth status tests ---

func TestRegistry_OAuthStatuses_Codex(t *testing.T) {
	r := NewRegistry()

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg)

	// Clear any stale tokens from disk (shared token file path).
	ts := r.GetTokenStore("codex")
	if ts != nil {
		ts.Clear()
	}

	statuses := r.OAuthStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}

	s := statuses[0]
	if s.BackendName != "codex" {
		t.Errorf("BackendName = %q, want %q", s.BackendName, "codex")
	}
	if s.BackendType != "codex" {
		t.Errorf("BackendType = %q, want %q", s.BackendType, "codex")
	}
	if s.Authenticated {
		t.Error("should not be authenticated without a token")
	}
	if !s.NeedsReauth {
		t.Error("should need re-auth without a token")
	}
}

func TestRegistry_OAuthStatuses_CodexWithToken(t *testing.T) {
	r := NewRegistry()

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg)

	// Clean up any stale token files from previous test runs.
	ts := r.GetTokenStore("codex")
	ts.Clear()

	// Save a valid token.
	ts.Save(&oauth.TokenData{
		AccessToken:  "valid-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	})

	statuses := r.OAuthStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}

	s := statuses[0]
	if !s.Authenticated {
		t.Error("should be authenticated with a valid token")
	}
	if s.NeedsReauth {
		t.Error("should not need re-auth with a valid token")
	}
	if s.TokenSource != "codex_oauth" {
		t.Errorf("TokenSource = %q, want %q", s.TokenSource, "codex_oauth")
	}
	if s.TokenExpiry == "" {
		t.Error("TokenExpiry should be set")
	}
}

func TestRegistry_OAuthStatuses_SkipsNonOAuth(t *testing.T) {
	r := NewRegistry()

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
	r.LoadFromConfig(cfg)

	statuses := r.OAuthStatuses()
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses for non-OAuth backend, got %d", len(statuses))
	}
}

func TestRegistry_OAuthStatuses_Mixed(t *testing.T) {
	r := NewRegistry()

	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}
	r.LoadFromConfig(cfg)

	statuses := r.OAuthStatuses()
	if len(statuses) != 2 {
		t.Errorf("expected 2 statuses (codex + copilot), got %d", len(statuses))
	}

	names := make(map[string]bool)
	for _, s := range statuses {
		names[s.BackendName] = true
	}
	if !names["codex"] || !names["copilot"] {
		t.Errorf("expected codex and copilot in statuses, got %v", names)
	}
}

func TestRegistry_HotReload_AddsCodexAtRuntime(t *testing.T) {
	r := NewRegistry()

	// Initial config without codex.
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

	if r.Has("codex") {
		t.Error("codex should not be registered initially")
	}

	// Simulate SIGHUP with codex added.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg2)

	if !r.Has("codex") {
		t.Error("codex should be registered after hot-reload")
	}

	// Verify the codex backend works.
	_, modelID, err := r.Resolve("codex/o4-mini")
	if err != nil {
		t.Fatalf("should resolve codex/o4-mini: %v", err)
	}
	if modelID != "o4-mini" {
		t.Errorf("modelID = %q, want %q", modelID, "o4-mini")
	}
}

func TestRegistry_HotReload_RemovesCodexAtRuntime(t *testing.T) {
	r := NewRegistry()

	// Initial config with codex.
	cfg1 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}
	r.LoadFromConfig(cfg1)

	if !r.Has("codex") {
		t.Error("codex should be registered initially")
	}

	// Simulate SIGHUP with codex removed.
	// Configure openrouter with explicit models so it doesn't wildcard-match codex models.
	cfg2 := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
				Models:  []string{"openai/gpt-4o"},
			},
		},
	}
	r.LoadFromConfig(cfg2)

	if r.Has("codex") {
		t.Error("codex should be removed after hot-reload")
	}
	if !r.Has("openrouter") {
		t.Error("openrouter should still be registered")
	}

	// Verify requests to codex backend fail.
	_, _, err := r.Resolve("codex/o4-mini")
	if err == nil {
		t.Error("expected error resolving codex/o4-mini after removal")
	}
}
