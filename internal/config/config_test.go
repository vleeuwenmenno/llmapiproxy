package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func boolPtr(b bool) *bool { return &b }

// --- OAuthConfig struct tests ---

func TestOAuthConfig_Fields(t *testing.T) {
	oauth := &OAuthConfig{
		ClientID:  "test-client-id",
		Scopes:    []string{"openid", "profile"},
		TokenPath: "/tmp/tokens/codex.json",
		AuthURL:   "https://auth.example.com/authorize",
		TokenURL:  "https://auth.example.com/token",
	}
	if oauth.ClientID != "test-client-id" {
		t.Errorf("ClientID = %q, want %q", oauth.ClientID, "test-client-id")
	}
	if len(oauth.Scopes) != 2 {
		t.Errorf("Scopes length = %d, want 2", len(oauth.Scopes))
	}
	if oauth.TokenPath != "/tmp/tokens/codex.json" {
		t.Errorf("TokenPath = %q, want %q", oauth.TokenPath, "/tmp/tokens/codex.json")
	}
	if oauth.AuthURL != "https://auth.example.com/authorize" {
		t.Errorf("AuthURL = %q, want %q", oauth.AuthURL, "https://auth.example.com/authorize")
	}
	if oauth.TokenURL != "https://auth.example.com/token" {
		t.Errorf("TokenURL = %q, want %q", oauth.TokenURL, "https://auth.example.com/token")
	}
}

func TestBackendConfig_OAuthField(t *testing.T) {
	bc := BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		OAuth: &OAuthConfig{
			ClientID: "my-client",
			AuthURL:  "https://auth.openai.com/authorize",
			TokenURL: "https://auth.openai.com/oauth/token",
		},
	}
	if bc.OAuth == nil {
		t.Fatal("OAuth field is nil")
	}
	if bc.OAuth.ClientID != "my-client" {
		t.Errorf("OAuth.ClientID = %q, want %q", bc.OAuth.ClientID, "my-client")
	}
}

// --- IsOAuthBackend tests ---

func TestIsOAuthBackend_Copilot(t *testing.T) {
	bc := BackendConfig{Type: "copilot"}
	if !bc.IsOAuthBackend() {
		t.Error("copilot should be an OAuth backend")
	}
}

func TestIsOAuthBackend_Codex(t *testing.T) {
	bc := BackendConfig{Type: "codex"}
	if !bc.IsOAuthBackend() {
		t.Error("codex should be an OAuth backend")
	}
}

func TestIsOAuthBackend_OpenAI(t *testing.T) {
	bc := BackendConfig{Type: "openai"}
	if bc.IsOAuthBackend() {
		t.Error("openai should not be an OAuth backend")
	}
}

func TestIsOAuthBackend_Empty(t *testing.T) {
	bc := BackendConfig{Type: ""}
	if bc.IsOAuthBackend() {
		t.Error("empty type should not be an OAuth backend")
	}
}

// --- Config validation tests ---

func TestValidate_OpenAIBackendRequiresAPIKey(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				// APIKey is empty — should fail
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing api_key on openai backend")
	}
}

func TestValidate_CopilotBackendDoesNotRequireAPIKey(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				// APIKey is empty — should be fine for copilot
			},
		},
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no validation error for copilot without api_key, got: %v", err)
	}
}

func TestValidate_CodexBackendDoesNotRequireAPIKey(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				// APIKey is empty — should be fine for codex
			},
		},
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("expected no validation error for codex without api_key, got: %v", err)
	}
}

func TestValidate_OAuthBackendStillRequiresBaseURL(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name: "copilot",
				Type: "copilot",
				// BaseURL is empty — should still fail
			},
		},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing base_url on copilot backend")
	}
}

func TestValidate_OAuthBackendWithAPIKeyAllowed(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				APIKey:  "some-key",
			},
		},
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("copilot with api_key should also be valid, got: %v", err)
	}
}

func TestValidate_MixedBackends(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
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
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("mixed backends should validate, got: %v", err)
	}
}

func TestValidate_DisabledOAuthBackendSkipped(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			APIKeys: []string{"test-key"},
		},
		Backends: []BackendConfig{
			{
				Name:    "openrouter",
				Type:    "openai",
				BaseURL: "https://openrouter.ai/api/v1",
				APIKey:  "sk-or-key",
			},
			{
				Name:    "disabled-copilot",
				Type:    "copilot",
				BaseURL: "",
				Enabled: boolPtr(false),
			},
		},
	}
	err := cfg.Validate()
	if err != nil {
		t.Fatalf("disabled backend should be skipped, got: %v", err)
	}
}

// --- YAML parsing tests ---

func TestParse_BackendWithOAuth(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: codex
    type: codex
    base_url: https://chatgpt.com/backend-api/codex
    oauth:
      client_id: "my-client-id"
      scopes:
        - "openid"
        - "profile"
      token_path: "/tmp/codex-token.json"
      auth_url: "https://auth.openai.com/authorize"
      token_url: "https://auth.openai.com/oauth/token"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	bc := cfg.Backends[0]
	if bc.OAuth == nil {
		t.Fatal("OAuth config is nil")
	}
	if bc.OAuth.ClientID != "my-client-id" {
		t.Errorf("ClientID = %q, want %q", bc.OAuth.ClientID, "my-client-id")
	}
	if len(bc.OAuth.Scopes) != 2 {
		t.Errorf("Scopes length = %d, want 2", len(bc.OAuth.Scopes))
	}
	if bc.OAuth.TokenPath != "/tmp/codex-token.json" {
		t.Errorf("TokenPath = %q, want %q", bc.OAuth.TokenPath, "/tmp/codex-token.json")
	}
	if bc.OAuth.AuthURL != "https://auth.openai.com/authorize" {
		t.Errorf("AuthURL = %q, want %q", bc.OAuth.AuthURL, "https://auth.openai.com/authorize")
	}
	if bc.OAuth.TokenURL != "https://auth.openai.com/oauth/token" {
		t.Errorf("TokenURL = %q, want %q", bc.OAuth.TokenURL, "https://auth.openai.com/oauth/token")
	}
}

func TestManager_SetGlobalIdentityProfile_PersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := `server:
  api_keys: ["test-key"]
identity_profile: opencode
backends:
  - name: openrouter
    type: openai
    base_url: https://example.com/v1
    api_key: test-backend-key
`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}

	if err := mgr.SetGlobalIdentityProfile("claude-code"); err != nil {
		t.Fatalf("SetGlobalIdentityProfile error: %v", err)
	}

	got := mgr.Get().IdentityProfile
	if got != "claude-code" {
		t.Fatalf("IdentityProfile = %q, want %q", got, "claude-code")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "identity_profile: claude-code") {
		t.Fatalf("config file missing persisted global identity_profile:\n%s", text)
	}

	if err := mgr.SetGlobalIdentityProfile("none"); err != nil {
		t.Fatalf("SetGlobalIdentityProfile(none) error: %v", err)
	}
	if mgr.Get().IdentityProfile != "none" {
		t.Fatalf("IdentityProfile after setting none = %q, want %q", mgr.Get().IdentityProfile, "none")
	}
}

func TestManager_SetBackendIdentityProfile_PersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := `server:
  api_keys: ["test-key"]
identity_profile: opencode
backends:
  - name: openrouter
    type: openai
    base_url: https://example.com/v1
    api_key: test-backend-key
    disabled_models:
      - foo
  - name: codex
    type: codex
    base_url: https://example.com/codex
    api_key: ""
`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	mgr, err := NewManager(path)
	if err != nil {
		t.Fatalf("NewManager error: %v", err)
	}

	if err := mgr.SetBackendIdentityProfile("openrouter", "claude-code"); err != nil {
		t.Fatalf("SetBackendIdentityProfile error: %v", err)
	}

	var openrouter *BackendConfig
	for i := range mgr.Get().Backends {
		if mgr.Get().Backends[i].Name == "openrouter" {
			openrouter = &mgr.Get().Backends[i]
			break
		}
	}
	if openrouter == nil {
		t.Fatal("openrouter backend not found after reload")
	}
	if openrouter.IdentityProfile != "claude-code" {
		t.Fatalf("backend IdentityProfile = %q, want %q", openrouter.IdentityProfile, "claude-code")
	}
	if len(openrouter.DisabledModels) != 1 || openrouter.DisabledModels[0] != "foo" {
		t.Fatalf("disabled_models not preserved after rewrite: %#v", openrouter.DisabledModels)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "identity_profile: claude-code") {
		t.Fatalf("config file missing persisted backend identity_profile:\n%s", text)
	}
	if !strings.Contains(text, "disabled_models:") {
		t.Fatalf("config file dropped disabled_models during rewrite:\n%s", text)
	}
}

func TestParse_BackendWithoutOAuth(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Backends[0].OAuth != nil {
		t.Error("OAuth should be nil for non-OAuth backend")
	}
}

func TestParse_CopilotNoAPIKey(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("copilot without api_key should parse, got: %v", err)
	}
	if cfg.Backends[0].Type != "copilot" {
		t.Errorf("Type = %q, want %q", cfg.Backends[0].Type, "copilot")
	}
}

func TestParse_DefaultTypeIsOpenAI(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: mybackend
    base_url: https://api.example.com/v1
    api_key: "my-key"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Backends[0].Type != "openai" {
		t.Errorf("Type = %q, want %q (default)", cfg.Backends[0].Type, "openai")
	}
}

// --- Manager hot-reload tests ---

func TestManager_HotReload_AddsOAuthBackend(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Initial config with one openai backend.
	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	cfg := mgr.Get()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}

	// Update config to add a copilot backend.
	updatedYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(updatedYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	cfg = mgr.Get()
	if len(cfg.Backends) != 2 {
		t.Fatalf("expected 2 backends after reload, got %d", len(cfg.Backends))
	}

	found := false
	for _, b := range cfg.Backends {
		if b.Name == "copilot" {
			found = true
			if b.Type != "copilot" {
				t.Errorf("copilot type = %q, want %q", b.Type, "copilot")
			}
		}
	}
	if !found {
		t.Error("copilot backend not found after reload")
	}
}

func TestManager_HotReload_RemovesOAuthBackend(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Initial config with openai + copilot.
	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Reload with copilot removed.
	updatedYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
`
	if err := os.WriteFile(cfgPath, []byte(updatedYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	cfg := mgr.Get()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend after removing copilot, got %d", len(cfg.Backends))
	}
	if cfg.Backends[0].Name != "openrouter" {
		t.Errorf("remaining backend = %q, want %q", cfg.Backends[0].Name, "openrouter")
	}
}

func TestManager_HotReload_UpdatesOAuthBackendConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Initial config with copilot backend.
	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	cfg := mgr.Get()
	if cfg.Backends[0].BaseURL != "https://api.githubcopilot.com" {
		t.Fatalf("initial BaseURL = %q, want %q", cfg.Backends[0].BaseURL, "https://api.githubcopilot.com")
	}

	// Update to use Business base URL.
	updatedYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: https://api.business.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(updatedYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	cfg = mgr.Get()
	if cfg.Backends[0].BaseURL != "https://api.business.githubcopilot.com" {
		t.Errorf("updated BaseURL = %q, want %q", cfg.Backends[0].BaseURL, "https://api.business.githubcopilot.com")
	}
}

func TestManager_HotReload_MultipleOAuthBackends(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
  - name: codex
    type: codex
    base_url: https://chatgpt.com/backend-api/codex
    oauth:
      client_id: "codex-client"
      auth_url: "https://auth.openai.com/authorize"
      token_url: "https://auth.openai.com/oauth/token"
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	cfg := mgr.Get()
	if len(cfg.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(cfg.Backends))
	}

	names := make(map[string]bool)
	for _, b := range cfg.Backends {
		names[b.Name] = true
	}
	for _, name := range []string{"openrouter", "copilot", "codex"} {
		if !names[name] {
			t.Errorf("backend %q not found", name)
		}
	}

	// Verify codex has OAuth config.
	for _, b := range cfg.Backends {
		if b.Name == "codex" {
			if b.OAuth == nil {
				t.Fatal("codex OAuth config is nil")
			}
			if b.OAuth.ClientID != "codex-client" {
				t.Errorf("codex ClientID = %q, want %q", b.OAuth.ClientID, "codex-client")
			}
		}
	}
}

func TestManager_OnChange_CalledOnReload(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Register a change handler that records the new backend count.
	var gotCount int
	mgr.OnChange(func(cfg *Config) {
		gotCount = len(cfg.Backends)
	})

	// Reload with an added copilot backend.
	updatedYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(updatedYAML), 0600); err != nil {
		t.Fatalf("writing updated config: %v", err)
	}

	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	if gotCount != 2 {
		t.Errorf("onChange received %d backends, want 2", gotCount)
	}
}

func TestBackendConfigModels_StringForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - gpt-4o
      - glm-5.1
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" {
		t.Errorf("models[0].ID = %q, want gpt-4o", models[0].ID)
	}
	if models[0].ContextLength != nil {
		t.Errorf("models[0].ContextLength = %v, want nil", models[0].ContextLength)
	}
	if models[1].ID != "glm-5.1" {
		t.Errorf("models[1].ID = %q, want glm-5.1", models[1].ID)
	}
}

func TestBackendConfigModels_ObjectForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - id: glm-5.1
        context_length: 131072
        max_output_tokens: 8192
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	m := models[0]
	if m.ID != "glm-5.1" {
		t.Errorf("ID = %q, want glm-5.1", m.ID)
	}
	if m.ContextLength == nil || *m.ContextLength != 131072 {
		t.Errorf("ContextLength = %v, want 131072", m.ContextLength)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %v, want 8192", m.MaxOutputTokens)
	}
}

func TestBackendConfigModels_MixedForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - gpt-4o
      - id: glm-5.1
        context_length: 128000
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" || models[0].ContextLength != nil {
		t.Errorf("unexpected models[0]: %+v", models[0])
	}
	if models[1].ID != "glm-5.1" || models[1].ContextLength == nil || *models[1].ContextLength != 128000 {
		t.Errorf("unexpected models[1]: %+v", models[1])
	}
}

func TestModelIDs(t *testing.T) {
	bc := BackendConfig{
		Models: []ModelConfig{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	ids := bc.ModelIDs()
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("ModelIDs() = %v", ids)

	}
}

func TestSwitchBackendType_OpenAIToAnthropic(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: mybackend
    type: openai
    base_url: https://api.example.com/v1
    api_key: "sk-test-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.SwitchBackendType("mybackend", "anthropic", "https://api.anthropic.com", ""); err != nil {
		t.Fatalf("SwitchBackendType: %v", err)
	}

	cfg := mgr.Get()
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	bc := cfg.Backends[0]
	if bc.Type != "anthropic" {
		t.Errorf("expected type anthropic, got %q", bc.Type)
	}
	if bc.BaseURL != "https://api.anthropic.com" {
		t.Errorf("expected base_url updated, got %q", bc.BaseURL)
	}
	if bc.APIKey != "sk-test-key" {
		t.Errorf("expected API key unchanged, got %q", bc.APIKey)
	}
}

func TestSwitchBackendType_AnthropicToOpenAI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: claude
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: "sk-ant-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if err := mgr.SwitchBackendType("claude", "openai", "", "new-key"); err != nil {
		t.Fatalf("SwitchBackendType: %v", err)
	}

	cfg := mgr.Get()
	bc := cfg.Backends[0]
	if bc.Type != "openai" {
		t.Errorf("expected type openai, got %q", bc.Type)
	}
	if bc.BaseURL != "https://api.anthropic.com" {
		t.Errorf("expected base_url unchanged when empty, got %q", bc.BaseURL)
	}
	if bc.APIKey != "new-key" {
		t.Errorf("expected API key updated, got %q", bc.APIKey)
	}
}

func TestSwitchBackendType_UnsupportedTargetType(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: mybackend
    type: openai
    base_url: https://api.example.com/v1
    api_key: "sk-test-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	err = mgr.SwitchBackendType("mybackend", "copilot", "", "")
	if err == nil {
		t.Fatal("expected error for unsupported target type")
	}
}

func TestSwitchBackendType_CannotSwitchCopilotBackend(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	err = mgr.SwitchBackendType("copilot", "openai", "", "")
	if err == nil {
		t.Fatal("expected error when trying to switch a copilot backend")
	}
}

func TestSwitchBackendType_NotFound(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	initialYAML := `
server:
  api_keys: ["test-key"]
backends:
  - name: mybackend
    type: openai
    base_url: https://api.example.com/v1
    api_key: "sk-test-key"
`
	if err := os.WriteFile(cfgPath, []byte(initialYAML), 0600); err != nil {
		t.Fatalf("writing initial config: %v", err)
	}

	mgr, err := NewManager(cfgPath)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	err = mgr.SwitchBackendType("nonexistent", "anthropic", "", "")
	if err == nil {
		t.Fatal("expected error for nonexistent backend")
	}
}

func TestParse_ModelAliases(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
    model_aliases:
      glm-5.1-precision: glm-5.1
      glm-5.1-speed: glm-5.1
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	bc := cfg.Backends[0]
	if len(bc.ModelAliases) != 2 {
		t.Fatalf("expected 2 model aliases, got %d", len(bc.ModelAliases))
	}
	if bc.ModelAliases["glm-5.1-precision"] != "glm-5.1" {
		t.Errorf("alias for glm-5.1-precision = %q, want %q", bc.ModelAliases["glm-5.1-precision"], "glm-5.1")
	}
	if bc.ModelAliases["glm-5.1-speed"] != "glm-5.1" {
		t.Errorf("alias for glm-5.1-speed = %q, want %q", bc.ModelAliases["glm-5.1-speed"], "glm-5.1")
	}
}

func TestParse_ModelAliases_EmptyOmitted(t *testing.T) {
	yaml := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if cfg.Backends[0].ModelAliases != nil {
		t.Errorf("expected nil ModelAliases, got %v", cfg.Backends[0].ModelAliases)
	}
}

func TestModelAliases_RoundTrip(t *testing.T) {
	original := `
server:
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: "sk-or-key"
    disabled_models:
      - foo
    model_aliases:
      glm-5.1-precision: glm-5.1
`
	cfg, err := Parse([]byte(original))
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	cfg2, err := Parse(data)
	if err != nil {
		t.Fatalf("Re-parse failed: %v", err)
	}

	bc := cfg2.Backends[0]
	if bc.ModelAliases["glm-5.1-precision"] != "glm-5.1" {
		t.Errorf("alias not preserved after round-trip: %v", bc.ModelAliases)
	}
	if len(bc.DisabledModels) != 1 || bc.DisabledModels[0] != "foo" {
		t.Errorf("disabled_models not preserved: %v", bc.DisabledModels)
	}
}
