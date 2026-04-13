package backend

import (
	"context"
	"io"
	"net/url"
	"strings"
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

func TestRegistry_LoadFromConfig_Anthropic(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "anthropic",
				Type:    "anthropic",
				BaseURL: "https://api.anthropic.com/v1",
				APIKey:  "sk-ant-test",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if !r.Has("anthropic") {
		t.Error("anthropic backend not registered")
	}
	b := r.Get("anthropic")
	if b == nil {
		t.Fatal("anthropic backend is nil")
	}
	if b.Name() != "anthropic" {
		t.Errorf("Name() = %q, want %q", b.Name(), "anthropic")
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
				Models:  []config.ModelConfig{{ID: "gpt-4o"}},
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
				Models:  []config.ModelConfig{{ID: "o3"}},
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
	// NeedsReauth is false when no token exists (it's 'not connected', not 'needs re-auth').
	if s.NeedsReauth {
		t.Error("should NOT need re-auth when never connected")
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
				Models:  []config.ModelConfig{{ID: "openai/gpt-4o"}},
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

// --- Redirect URI derivation tests ---

func TestRegistry_CodexBackend_DerivedRedirectURI(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":9000",
		},
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

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend, ok := b.(*CodexBackend)
	if !ok {
		t.Fatal("backend is not a *CodexBackend")
	}

	handler := codexBackend.GetOAuthHandler()
	authURL, _, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	redirectURI := u.Query().Get("redirect_uri")
	expected := oauth.BuiltinCodexRedirectURI()
	if redirectURI != expected {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, expected)
	}
}

func TestRegistry_CodexBackend_DefaultListenAddr(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: "",
		},
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

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend, ok := b.(*CodexBackend)
	if !ok {
		t.Fatal("backend is not a *CodexBackend")
	}

	handler := codexBackend.GetOAuthHandler()
	authURL, _, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	redirectURI := u.Query().Get("redirect_uri")
	expected := oauth.BuiltinCodexRedirectURI()
	if redirectURI != expected {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, expected)
	}
}

func TestRegistry_CodexBackend_BuiltinClientIgnoresBackendNameInRedirectURI(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8000",
		},
		Backends: []config.BackendConfig{
			{
				Name:    "my-codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	b := r.Get("my-codex")
	if b == nil {
		t.Fatal("my-codex backend is nil")
	}

	codexBackend, ok := b.(*CodexBackend)
	if !ok {
		t.Fatal("backend is not a *CodexBackend")
	}

	handler := codexBackend.GetOAuthHandler()
	authURL, _, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	redirectURI := u.Query().Get("redirect_uri")
	expected := oauth.BuiltinCodexRedirectURI()
	if redirectURI != expected {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, expected)
	}
}

func TestRegistry_CodexBackend_CustomClientUsesUIRedirectURI(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8123",
		},
		Backends: []config.BackendConfig{
			{
				Name:    "my-codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				OAuth: &config.OAuthConfig{
					ClientID: "custom-client-id",
				},
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	b := r.Get("my-codex")
	if b == nil {
		t.Fatal("my-codex backend is nil")
	}

	codexBackend := b.(*CodexBackend)
	authURL, _, err := codexBackend.GetOAuthHandler().AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	redirectURI := u.Query().Get("redirect_uri")
	expected := "http://localhost:8123/ui/oauth/callback/my-codex"
	if redirectURI != expected {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, expected)
	}
}

func TestRegistry_CodexBackend_IgnoresPlaceholderClientID(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8000",
		},
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				OAuth: &config.OAuthConfig{
					ClientID: "your-codex-client-id",
				},
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend := b.(*CodexBackend)
	authURL, _, err := codexBackend.GetOAuthHandler().AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	gotClientID := u.Query().Get("client_id")
	if gotClientID == "your-codex-client-id" {
		t.Fatalf("placeholder client_id leaked into authorize URL")
	}
	if gotClientID == "" {
		t.Fatalf("expected built-in client_id to be used")
	}
}

func TestRegistry_CodexBackend_NormalizesLegacyAuthURL(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8000",
		},
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				OAuth: &config.OAuthConfig{
					AuthURL: "https://auth.openai.com/authorize",
				},
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend := b.(*CodexBackend)
	authURL, _, err := codexBackend.GetOAuthHandler().AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://auth.openai.com/oauth/authorize" {
		t.Fatalf("normalized auth url = %q, want %q", got, "https://auth.openai.com/oauth/authorize")
	}
}

func TestRegistry_CodexBackend_AppendsOfflineAccessScope(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: ":8000",
		},
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				OAuth: &config.OAuthConfig{
					Scopes: []string{"openid", "profile", "email"},
				},
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend := b.(*CodexBackend)
	authURL, _, err := codexBackend.GetOAuthHandler().AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	if got := u.Query().Get("scope"); !strings.Contains(got, "offline_access") {
		t.Fatalf("expected offline_access scope to be appended, got %q", got)
	}
}

func TestRegistry_CodexBackend_WildcardListenAddr(t *testing.T) {
	cfg := &config.Config{
		Server: config.ServerConfig{
			Listen: "0.0.0.0:8080",
		},
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

	b := r.Get("codex")
	if b == nil {
		t.Fatal("codex backend is nil")
	}

	codexBackend, ok := b.(*CodexBackend)
	if !ok {
		t.Fatal("backend is not a *CodexBackend")
	}

	handler := codexBackend.GetOAuthHandler()
	authURL, _, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	redirectURI := u.Query().Get("redirect_uri")
	expected := oauth.BuiltinCodexRedirectURI()
	if redirectURI != expected {
		t.Errorf("redirect_uri = %q, want %q", redirectURI, expected)
	}
}

// --- OAuthStatuses ordering test ---

func TestOAuthStatuses_OrderingDeterministic(t *testing.T) {
	r := NewRegistry()

	// Configure multiple OAuth backends with names that are likely to
	// be in different hash-map positions (so non-deterministic iteration
	// would shuffle them on some runs).
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "zebra-copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
			{
				Name:    "alpha-codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
			},
			{
				Name:    "mid-backend",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
			},
		},
	}
	r.LoadFromConfig(cfg)

	statuses := r.OAuthStatuses()
	if len(statuses) != 3 {
		t.Fatalf("expected 3 statuses, got %d", len(statuses))
	}

	// Verify statuses are sorted alphabetically by BackendName.
	for i := 1; i < len(statuses); i++ {
		if statuses[i].BackendName < statuses[i-1].BackendName {
			names := make([]string, len(statuses))
			for j, s := range statuses {
				names[j] = s.BackendName
			}
			t.Errorf("statuses not sorted: %v (status %d (%q) < status %d (%q))",
				names, i, statuses[i].BackendName, i-1, statuses[i-1].BackendName)
		}
	}

	// Specifically verify the expected order.
	expected := []string{"alpha-codex", "mid-backend", "zebra-copilot"}
	for i, s := range statuses {
		if s.BackendName != expected[i] {
			t.Errorf("status[%d].BackendName = %q, want %q", i, s.BackendName, expected[i])
		}
	}
}

func TestOAuthStatuses_SingleBackend(t *testing.T) {
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

	statuses := r.OAuthStatuses()
	if len(statuses) != 1 {
		t.Fatalf("expected 1 status, got %d", len(statuses))
	}
	if statuses[0].BackendName != "copilot" {
		t.Errorf("BackendName = %q, want %q", statuses[0].BackendName, "copilot")
	}
}

func TestOAuthStatuses_Empty(t *testing.T) {
	r := NewRegistry()
	statuses := r.OAuthStatuses()
	if len(statuses) != 0 {
		t.Errorf("expected 0 statuses, got %d", len(statuses))
	}
}

// --- routeMock implements Backend for ResolveRoute tests ---

type routeMock struct {
	name   string
	models map[string]bool
}

func (m *routeMock) Name() string { return m.name }
func (m *routeMock) SupportsModel(modelID string) bool {
	return m.models[modelID]
}
func (m *routeMock) ChatCompletion(_ context.Context, _ *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (m *routeMock) ChatCompletionStream(_ context.Context, _ *ChatCompletionRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (m *routeMock) ListModels(_ context.Context) ([]Model, error) { return nil, nil }
func (m *routeMock) ClearModelCache()                              {}
func (m *routeMock) ResolveModelID(canonicalID string) string      { return canonicalID }

// --- ResolveRoute tests ---

func TestResolveRoute_ExplicitConfig_SingleBackend(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai"}},
		},
	}

	entries, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "openai")
	}
	if entries[0].ModelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "gpt-4o")
	}
	if strategy != config.StrategyPriority {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyPriority)
	}
}

func TestResolveRoute_ExplicitConfig_MultiBackend(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai"})
	r.RegisterBackend("openrouter", &routeMock{name: "openrouter"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai", "openrouter"}},
		},
	}

	entries, _, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("entries[0] = %q, want %q", entries[0].Backend.Name(), "openai")
	}
	if entries[1].Backend.Name() != "openrouter" {
		t.Errorf("entries[1] = %q, want %q", entries[1].Backend.Name(), "openrouter")
	}
}

func TestResolveRoute_ExplicitConfig_PerModelStrategy(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})
	r.RegisterBackend("b", &routeMock{name: "b"})

	routing := config.RoutingConfig{
		Strategy: config.StrategyPriority,
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a", "b"}, Strategy: config.StrategyRace},
		},
	}

	_, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strategy != config.StrategyRace {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyRace)
	}
}

func TestResolveRoute_ExplicitConfig_FallsBackToGlobalStrategy(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})

	routing := config.RoutingConfig{
		Strategy: config.StrategyRace,
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a"}},
		},
	}

	_, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strategy != config.StrategyRace {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyRace)
	}
}

func TestResolveRoute_ExplicitConfig_DefaultStrategyPriority(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a"}},
		},
	}

	_, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strategy != config.StrategyPriority {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyPriority)
	}
}

func TestResolveRoute_ExplicitConfig_StaggerDelay_PerModel(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})
	r.RegisterBackend("b", &routeMock{name: "b"})

	routing := config.RoutingConfig{
		StaggerDelayMs: 100,
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a", "b"}, Strategy: config.StrategyStaggeredRace, StaggerDelayMs: 300},
		},
	}

	_, _, staggerMs, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if staggerMs != 300 {
		t.Errorf("staggerDelayMs = %d, want 300", staggerMs)
	}
}

func TestResolveRoute_ExplicitConfig_StaggerDelay_FallsBackToGlobal(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})

	routing := config.RoutingConfig{
		StaggerDelayMs: 250,
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a"}, Strategy: config.StrategyStaggeredRace},
		},
	}

	_, _, staggerMs, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if staggerMs != 250 {
		t.Errorf("staggerDelayMs = %d, want 250", staggerMs)
	}
}

func TestResolveRoute_ExplicitConfig_SkipsUnregisteredBackends(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai"})
	// "ghost" is NOT registered

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"ghost", "openai"}},
		},
	}

	entries, _, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (ghost skipped), got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "openai")
	}
}

func TestResolveRoute_ExplicitConfig_AllBackendsUnregistered_FallsThrough(t *testing.T) {
	r := NewRegistry()
	// No backends registered at all, but config references them.

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"ghost1", "ghost2"}},
		},
	}

	// All config backends are unregistered → skip this model entry,
	// fall through to prefix/wildcard. No backends exist → error.
	_, _, _, err := r.ResolveRoute("gpt-4o", routing)
	if err == nil {
		t.Fatal("expected error when all configured backends are unregistered")
	}
}

func TestResolveRoute_ExplicitConfig_RoundRobin_RotatesEntries(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})
	r.RegisterBackend("b", &routeMock{name: "b"})
	r.RegisterBackend("c", &routeMock{name: "c"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a", "b", "c"}, Strategy: config.StrategyRoundRobin},
		},
	}

	// Call ResolveRoute 3 times; each call should yield a different leading backend.
	leaders := make([]string, 3)
	for i := 0; i < 3; i++ {
		entries, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if strategy != config.StrategyRoundRobin {
			t.Errorf("call %d: strategy = %q, want %q", i, strategy, config.StrategyRoundRobin)
		}
		if len(entries) != 3 {
			t.Fatalf("call %d: expected 3 entries, got %d", i, len(entries))
		}
		leaders[i] = entries[0].Backend.Name()
	}

	// All three leaders should be distinct.
	seen := map[string]bool{}
	for _, l := range leaders {
		seen[l] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct leaders, got %v (leaders: %v)", seen, leaders)
	}
}

func TestResolveRoute_ExplicitConfig_RoundRobin_PerModelCounters(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("a", &routeMock{name: "a"})
	r.RegisterBackend("b", &routeMock{name: "b"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "model-x", Backends: []string{"a", "b"}, Strategy: config.StrategyRoundRobin},
			{Model: "model-y", Backends: []string{"a", "b"}, Strategy: config.StrategyRoundRobin},
		},
	}

	// Call model-x twice, then model-y once.
	// model-y should start from its own counter (independent of model-x).
	e1, _, _, _ := r.ResolveRoute("model-x", routing)
	e2, _, _, _ := r.ResolveRoute("model-x", routing)
	ey, _, _, _ := r.ResolveRoute("model-y", routing)

	// model-x calls should have different leaders.
	if e1[0].Backend.Name() == e2[0].Backend.Name() {
		t.Errorf("model-x: calls 1 and 2 should have different leaders, both got %q", e1[0].Backend.Name())
	}

	// model-y's first call should start from the same leader as model-x's first call
	// (both counters start at 0).
	if ey[0].Backend.Name() != e1[0].Backend.Name() {
		t.Errorf("model-y first call leader = %q, expected same as model-x first call = %q", ey[0].Backend.Name(), e1[0].Backend.Name())
	}
}

// --- Prefix routing tests ---

func TestResolveRoute_PrefixRouting_Basic(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openrouter", &routeMock{name: "openrouter"})

	entries, strategy, _, err := r.ResolveRoute("openrouter/gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openrouter" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "openrouter")
	}
	if entries[0].ModelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "gpt-4o")
	}
	if strategy != config.StrategyPriority {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyPriority)
	}
}

func TestResolveRoute_PrefixRouting_NestedSlash(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openrouter", &routeMock{name: "openrouter"})

	// "openrouter/openai/gpt-4o" — prefix is "openrouter", model is "openai/gpt-4o"
	entries, _, _, err := r.ResolveRoute("openrouter/openai/gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].ModelID != "openai/gpt-4o" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "openai/gpt-4o")
	}
}

func TestResolveRoute_PrefixRouting_UnknownBackend_FallsToWildcard(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"unknown/gpt-4o": true}})

	// "unknown/gpt-4o" — "unknown" is not a backend name, so prefix fails.
	// Then wildcard: openai supports "unknown/gpt-4o" via its model list.
	entries, _, _, err := r.ResolveRoute("unknown/gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "openai")
	}
	if entries[0].ModelID != "unknown/gpt-4o" {
		t.Errorf("modelID = %q, want %q (wildcard should not strip prefix)", entries[0].ModelID, "unknown/gpt-4o")
	}
}

func TestResolveRoute_PrefixRouting_ExplicitConfigTakesPriority(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai"})
	r.RegisterBackend("openrouter", &routeMock{name: "openrouter"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			// Explicit config for "openrouter/gpt-4o" — should use openai, not prefix routing.
			{Model: "openrouter/gpt-4o", Backends: []string{"openai"}},
		},
	}

	entries, _, _, err := r.ResolveRoute("openrouter/gpt-4o", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("backend = %q, want %q (explicit config should override prefix)", entries[0].Backend.Name(), "openai")
	}
}

// --- Wildcard routing tests ---

func TestResolveRoute_Wildcard_SingleMatch(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"gpt-4o": true}})

	entries, strategy, _, err := r.ResolveRoute("gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "openai")
	}
	if entries[0].ModelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "gpt-4o")
	}
	if strategy != config.StrategyPriority {
		t.Errorf("strategy = %q, want %q", strategy, config.StrategyPriority)
	}
}

func TestResolveRoute_Wildcard_NoMatch(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"gpt-4o": true}})

	_, _, _, err := r.ResolveRoute("nonexistent-model", config.RoutingConfig{})
	if err == nil {
		t.Fatal("expected error for unsupported model")
	}
}

func TestResolveRoute_Wildcard_EmptyCache_ReturnsFalse(t *testing.T) {
	r := NewRegistry()
	// Backend with no models → SupportsModel returns false for everything.
	r.RegisterBackend("openai", &routeMock{name: "openai", models: nil})

	_, _, _, err := r.ResolveRoute("gpt-4o", config.RoutingConfig{})
	if err == nil {
		t.Fatal("expected error when no backend supports the model")
	}
}

func TestResolveRoute_NoBackends_ReturnsError(t *testing.T) {
	r := NewRegistry()

	_, _, _, err := r.ResolveRoute("gpt-4o", config.RoutingConfig{})
	if err == nil {
		t.Fatal("expected error with no backends registered")
	}
}

// --- Mixed backend scenarios ---

func TestResolveRoute_ThreeBackends_OpenAI_Codex_Copilot(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"gpt-4o": true}})
	r.RegisterBackend("codex", &routeMock{name: "codex", models: map[string]bool{"o4-mini": true}})
	r.RegisterBackend("copilot", &routeMock{name: "copilot", models: map[string]bool{"gpt-4o": true, "claude-sonnet-4": true}})

	routing := config.RoutingConfig{
		Strategy: config.StrategyPriority,
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai", "copilot"}},
			{Model: "o4-mini", Backends: []string{"codex"}},
		},
	}

	// gpt-4o should resolve to openai + copilot
	entries, _, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("gpt-4o: unexpected error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("gpt-4o: expected 2 entries, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" || entries[1].Backend.Name() != "copilot" {
		t.Errorf("gpt-4o: backends = [%q, %q], want [openai, copilot]", entries[0].Backend.Name(), entries[1].Backend.Name())
	}

	// o4-mini should resolve to codex only
	entries, _, _, err = r.ResolveRoute("o4-mini", routing)
	if err != nil {
		t.Fatalf("o4-mini: unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Backend.Name() != "codex" {
		t.Errorf("o4-mini: expected [codex], got %v", entries)
	}

	// claude-sonnet-4 — not in explicit config, should wildcard to copilot
	entries, _, _, err = r.ResolveRoute("claude-sonnet-4", routing)
	if err != nil {
		t.Fatalf("claude-sonnet-4: unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Backend.Name() != "copilot" {
		t.Errorf("claude-sonnet-4: expected [copilot], got %v", entries)
	}
}

func TestResolveRoute_FourBackends_2OpenAI_Anthropic_Copilot(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai-1", &routeMock{name: "openai-1", models: map[string]bool{"gpt-4o": true}})
	r.RegisterBackend("openai-2", &routeMock{name: "openai-2", models: map[string]bool{"gpt-4o": true}})
	r.RegisterBackend("anthropic", &routeMock{name: "anthropic", models: map[string]bool{"claude-sonnet-4": true}})
	r.RegisterBackend("copilot", &routeMock{name: "copilot", models: map[string]bool{"gpt-4o": true}})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai-1", "openai-2", "copilot"}, Strategy: config.StrategyRoundRobin},
			{Model: "claude-sonnet-4", Backends: []string{"anthropic"}},
		},
	}

	// gpt-4o with round-robin: 3 entries, should rotate.
	entries, strategy, _, err := r.ResolveRoute("gpt-4o", routing)
	if err != nil {
		t.Fatalf("gpt-4o: unexpected error: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("gpt-4o: expected 3 entries, got %d", len(entries))
	}
	if strategy != config.StrategyRoundRobin {
		t.Errorf("gpt-4o: strategy = %q, want %q", strategy, config.StrategyRoundRobin)
	}

	// claude-sonnet-4: single anthropic
	entries, strategy, _, err = r.ResolveRoute("claude-sonnet-4", routing)
	if err != nil {
		t.Fatalf("claude-sonnet-4: unexpected error: %v", err)
	}
	if len(entries) != 1 || entries[0].Backend.Name() != "anthropic" {
		t.Errorf("claude-sonnet-4: expected [anthropic], got %v", entries)
	}
	if strategy != config.StrategyPriority {
		t.Errorf("claude-sonnet-4: strategy = %q, want %q (default)", strategy, config.StrategyPriority)
	}
}

func TestResolveRoute_OfflineBackend_EmptyModelCache(t *testing.T) {
	r := NewRegistry()
	// "offline" backend has no models (simulating unreachable API / empty cache).
	r.RegisterBackend("offline", &routeMock{name: "offline", models: nil})
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"gpt-4o": true}})

	// Without explicit config, wildcard should skip "offline" and find "openai".
	// Note: map iteration is non-deterministic, so we can only test that it resolves.
	entries, _, _, err := r.ResolveRoute("gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Backend.Name() != "openai" {
		t.Errorf("expected openai, got %q", entries[0].Backend.Name())
	}
}

func TestResolveRoute_MissingModel_AllBackends(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai", models: map[string]bool{"gpt-4o": true}})
	r.RegisterBackend("anthropic", &routeMock{name: "anthropic", models: map[string]bool{"claude-sonnet-4": true}})
	r.RegisterBackend("copilot", &routeMock{name: "copilot", models: map[string]bool{"gpt-4o": true}})

	// Request a model that no backend supports.
	_, _, _, err := r.ResolveRoute("nonexistent-model", config.RoutingConfig{})
	if err == nil {
		t.Fatal("expected error when no backend supports the model")
	}
}

func TestResolveRoute_PrefixBypassesOfflineBackend(t *testing.T) {
	r := NewRegistry()
	// Even if backend has no models, prefix routing always works.
	r.RegisterBackend("openai", &routeMock{name: "openai", models: nil})

	entries, _, _, err := r.ResolveRoute("openai/gpt-4o", config.RoutingConfig{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].ModelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "gpt-4o")
	}
}

func TestResolveRoute_ExplicitConfig_ModelNotInConfig_UsesPrefix(t *testing.T) {
	r := NewRegistry()
	r.RegisterBackend("openai", &routeMock{name: "openai"})
	r.RegisterBackend("anthropic", &routeMock{name: "anthropic"})

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai"}},
		},
	}

	// claude-sonnet-4 is not in routing config; use prefix routing.
	entries, _, _, err := r.ResolveRoute("anthropic/claude-sonnet-4", routing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if entries[0].Backend.Name() != "anthropic" {
		t.Errorf("backend = %q, want %q", entries[0].Backend.Name(), "anthropic")
	}
	if entries[0].ModelID != "claude-sonnet-4" {
		t.Errorf("modelID = %q, want %q", entries[0].ModelID, "claude-sonnet-4")
	}
}
