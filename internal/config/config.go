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

// ModelConfig specifies a single model with optional metadata overrides.
// It supports both shorthand string form ("gpt-4o") and object form:
//
//	- id: gpt-4o
//	  context_length: 128000
//	  max_output_tokens: 16384
type ModelConfig struct {
	ID              string `yaml:"id"`
	ContextLength   *int64 `yaml:"context_length,omitempty"`
	MaxOutputTokens *int64 `yaml:"max_output_tokens,omitempty"`
}

// BackendConfig holds the configuration for a single backend provider.
type BackendConfig struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	BaseURL      string            `yaml:"base_url"`
	APIKey       string            `yaml:"api_key"`
	ExtraHeaders map[string]string `yaml:"extra_headers,omitempty"`
	Models       []ModelConfig     `yaml:"models,omitempty"`
	Enabled      *bool             `yaml:"enabled,omitempty"`
}

// ModelIDs returns the list of model IDs as plain strings (backward compat).
func (b *BackendConfig) ModelIDs() []string {
	ids := make([]string, len(b.Models))
	for i, m := range b.Models {
		ids[i] = m.ID
	}
	return ids
}

// UnmarshalYAML implements custom YAML unmarshaling for BackendConfig
// to support both "- model-name" (string) and "- id: model-name" (object)
// in the models list.
func (bc *BackendConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Use a shadow type to avoid infinite recursion.
	type raw struct {
		Name         string            `yaml:"name"`
		Type         string            `yaml:"type"`
		BaseURL      string            `yaml:"base_url"`
		APIKey       string            `yaml:"api_key"`
		ExtraHeaders map[string]string `yaml:"extra_headers,omitempty"`
		ModelsRaw    interface{}       `yaml:"models,omitempty"`
		Enabled      *bool             `yaml:"enabled,omitempty"`
	}
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	bc.Name = r.Name
	bc.Type = r.Type
	bc.BaseURL = r.BaseURL
	bc.APIKey = r.APIKey
	bc.ExtraHeaders = r.ExtraHeaders
	bc.Enabled = r.Enabled

	if r.ModelsRaw != nil {
		models, err := parseModelsField(r.ModelsRaw)
		if err != nil {
			return fmt.Errorf("backends[%s].models: %w", r.Name, err)
		}
		bc.Models = models
	}
	return nil
}

// parseModelsField handles both string and object entries in the models list.
func parseModelsField(raw interface{}) ([]ModelConfig, error) {
	// YAML unmarshals a list of strings as []interface{} containing strings,
	// and a list of maps as []interface{} containing map[string]interface{}.
	list, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected a list")
	}
	result := make([]ModelConfig, 0, len(list))
	for i, item := range list {
		switch v := item.(type) {
		case string:
			result = append(result, ModelConfig{ID: v})
		case map[string]interface{}:
			mc := ModelConfig{}
			if id, ok := v["id"].(string); ok {
				mc.ID = id
			} else {
				return nil, fmt.Errorf("entry %d: missing or non-string 'id' field", i)
			}
			if cl, ok := v["context_length"].(int); ok {
				val := int64(cl)
				mc.ContextLength = &val
			}
			if mot, ok := v["max_output_tokens"].(int); ok {
				val := int64(mot)
				mc.MaxOutputTokens = &val
			}
			result = append(result, mc)
		default:
			return nil, fmt.Errorf("entry %d: expected string or map, got %T", i, item)
		}
	}
	return result, nil
}

// IsEnabled returns true unless the backend is explicitly disabled.
func (b *BackendConfig) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
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
		if b.APIKey == "" {
			return fmt.Errorf("backends[%d].api_key: must not be empty for enabled backend", i)
		}
		if b.Type == "" {
			c.Backends[i].Type = "openai"
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
