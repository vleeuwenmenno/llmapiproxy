package config

import (
	"crypto/subtle"
	"fmt"
	"net/url"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig    `yaml:"server"`
	Backends []BackendConfig `yaml:"backends"`
	Clients  []ClientConfig  `yaml:"clients,omitempty"`
	Routing  RoutingConfig   `yaml:"routing,omitempty"`
}

type ClientConfig struct {
	Name        string            `yaml:"name"`
	APIKey      string            `yaml:"api_key"`
	BackendKeys map[string]string `yaml:"backend_keys,omitempty"`
}

type ModelRoutingConfig struct {
	Model    string   `yaml:"model"`
	Backends []string `yaml:"backends"`
}

type RoutingConfig struct {
	Models []ModelRoutingConfig `yaml:"models,omitempty"`
}

type ServerConfig struct {
	Listen       string   `yaml:"listen"`
	APIKeys      []string `yaml:"api_keys"`
	AdminKey     string   `yaml:"admin_key"`
	StatsPath    string   `yaml:"stats_path"`
	DisableStats bool     `yaml:"disable_stats"`
}

// OAuthConfig holds OAuth-related configuration for backends that use
// OAuth authentication (e.g., GitHub Copilot, OpenAI Codex) instead of
// static API keys.
type OAuthConfig struct {
	ClientID  string   `yaml:"client_id,omitempty"`
	Scopes    []string `yaml:"scopes,omitempty"`
	TokenPath string   `yaml:"token_path,omitempty"`
	AuthURL   string   `yaml:"auth_url,omitempty"`
	TokenURL  string   `yaml:"token_url,omitempty"`
}

type BackendConfig struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	ExtraHeaders map[string]string `yaml:"extra_headers,omitempty"`
	Models       []string          `yaml:"models,omitempty"`
	Enabled      *bool             `yaml:"enabled,omitempty"`
	OAuth        *OAuthConfig      `yaml:"oauth,omitempty"`
}

// IsEnabled returns true unless the backend is explicitly disabled.
func (b *BackendConfig) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

// IsOAuthBackend returns true if the backend type uses OAuth authentication
// rather than a static API key. OAuth backends discover tokens at runtime
// and do not require an api_key in the configuration.
func (b *BackendConfig) IsOAuthBackend() bool {
	switch b.Type {
	case "copilot", "codex":
		return true
	default:
		return false
	}
}

func (c *Config) Validate() error {
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.StatsPath == "" {
		c.Server.StatsPath = "stats.db"
	}
	if len(c.Server.APIKeys) == 0 && len(c.Clients) == 0 {
		return fmt.Errorf("server.api_keys: at least one API key is required")
	}
	enabledCount := 0
	for _, b := range c.Backends {
		if b.IsEnabled() {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		return fmt.Errorf("backends: at least one enabled backend is required")
	}
	for i, b := range c.Backends {
		if b.Name == "" {
			return fmt.Errorf("backends[%d].name: must not be empty", i)
		}
		if !b.IsEnabled() {
			continue
		}
		if b.BaseURL == "" {
			return fmt.Errorf("backends[%d].base_url: must not be empty for enabled backend", i)
		}
		if _, err := url.Parse(b.BaseURL); err != nil {
			return fmt.Errorf("backends[%d].base_url: invalid URL: %w", i, err)
		}
		// Default type to "openai" if not specified.
		if b.Type == "" {
			c.Backends[i].Type = "openai"
		}
		// OAuth backends (copilot, codex) do not require an api_key.
		// They authenticate via local token discovery or OAuth flows.
		if !c.Backends[i].IsOAuthBackend() && b.APIKey == "" {
			return fmt.Errorf("backends[%d].api_key: must not be empty for enabled backend", i)
		}
	}
	return nil
}

func (c *Config) LookupClient(token string) *ClientConfig {
	for i := range c.Clients {
		if subtle.ConstantTimeCompare([]byte(token), []byte(c.Clients[i].APIKey)) == 1 {
			return &c.Clients[i]
		}
	}
	for _, k := range c.Server.APIKeys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
			return &ClientConfig{Name: ""}
		}
	}
	return nil
}

func (c *Config) AllAPIKeys() []string {
	keys := make([]string, 0, len(c.Server.APIKeys)+len(c.Clients))
	keys = append(keys, c.Server.APIKeys...)
	for _, cl := range c.Clients {
		keys = append(keys, cl.APIKey)
	}
	return keys
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}
	return &cfg, nil
}

// Manager holds the current config and supports atomic reload.
type Manager struct {
	mu       sync.RWMutex
	path     string
	current  *Config
	onChange []func(*Config)
}

func NewManager(path string) (*Manager, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	return &Manager{path: path, current: cfg}, nil
}

func (m *Manager) Get() *Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.current
}

func (m *Manager) Reload() error {
	cfg, err := Load(m.path)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.current = cfg
	handlers := m.onChange
	m.mu.Unlock()

	for _, fn := range handlers {
		fn(cfg)
	}
	return nil
}

func (m *Manager) OnChange(fn func(*Config)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = append(m.onChange, fn)
}

func (m *Manager) Path() string {
	return m.path
}

// SaveRaw writes raw YAML bytes to the config file after validating them.
func (m *Manager) SaveRaw(data []byte) error {
	if _, err := Parse(data); err != nil {
		return err
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// UpdateAPIKeys replaces the server.api_keys list, persists the file, and reloads.
func (m *Manager) UpdateAPIKeys(keys []string) error {
	m.mu.Lock()
	m.current.Server.APIKeys = keys
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// ToggleBackend sets the enabled state of a backend by name and persists.
func (m *Manager) ToggleBackend(name string, enabled bool) error {
	m.mu.Lock()
	found := false
	for i, b := range m.current.Backends {
		if b.Name == name {
			m.current.Backends[i].Enabled = &[]bool{enabled}[0]
			found = true
			break
		}
	}
	cfg := m.current
	m.mu.Unlock()

	if !found {
		return fmt.Errorf("backend %q not found", name)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

func (m *Manager) UpdateClients(clients []ClientConfig) error {
	m.mu.Lock()
	m.current.Clients = clients
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

func (m *Manager) SaveRouting(routing RoutingConfig) error {
	m.mu.Lock()
	m.current.Routing = routing
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}
