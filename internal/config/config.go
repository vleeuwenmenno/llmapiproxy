package config

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Server ServerConfig `yaml:"server"`

	// IdentityProfile sets the global default identity profile for outgoing
	// requests. When empty or "none", no spoofing is applied. Each backend
	// can override this with its own identity_profile setting.
	IdentityProfile string `yaml:"identity_profile,omitempty"`

	// CustomIdentityProfiles defines user-created identity profiles in addition
	// to the built-in ones (codex-cli, gemini-cli, copilot-vscode, etc.).
	CustomIdentityProfiles []CustomIdentityProfile `yaml:"custom_identity_profiles,omitempty"`

	Backends []BackendConfig `yaml:"backends"`
	Clients  []ClientConfig  `yaml:"clients,omitempty"`
	Routing  RoutingConfig   `yaml:"routing,omitempty"`
}

// CustomIdentityProfile defines a user-created identity profile.
type CustomIdentityProfile struct {
	ID          string            `yaml:"id"`
	DisplayName string            `yaml:"display_name"`
	UserAgent   string            `yaml:"user_agent,omitempty"`
	Headers     map[string]string `yaml:"headers,omitempty"`
}

type ClientConfig struct {
	Name        string            `yaml:"name"`
	APIKey      string            `yaml:"api_key"`
	BackendKeys map[string]string `yaml:"backend_keys,omitempty"`
}

// Valid routing strategy values.
const (
	StrategyPriority      = "priority"
	StrategyRoundRobin    = "round-robin"
	StrategyRace          = "race"
	StrategyStaggeredRace = "staggered-race"
)

type ModelRoutingConfig struct {
	Model    string   `yaml:"model"`
	Backends []string `yaml:"backends"`
	// Strategy overrides the global routing strategy for this model.
	// Valid values: "priority", "round-robin", "race", "staggered-race". Empty = use global default.
	Strategy string `yaml:"strategy,omitempty"`
	// StaggerDelayMs is the delay between backend launches for the staggered-race strategy.
	// Defaults to 500ms when 0.
	StaggerDelayMs int `yaml:"stagger_delay_ms,omitempty" json:"stagger_delay_ms,omitempty"`
	// DisabledBackends lists backends that should be skipped for this model.
	// The backends remain in the Backends list for ordering/visibility but are
	// excluded from actual routing. Useful to temporarily opt out of specific
	// backends (e.g. plan-based providers) for a single model.
	DisabledBackends []string `yaml:"disabled_backends,omitempty" json:"disabled_backends,omitempty"`
}

type RoutingConfig struct {
	Models []ModelRoutingConfig `yaml:"models,omitempty"`
	// Strategy sets the default routing strategy when a model has multiple backends.
	// Valid values: "priority" (default), "round-robin", "race", "staggered-race".
	Strategy string `yaml:"strategy,omitempty"`
	// StaggerDelayMs is the default delay between backend launches for the staggered-race strategy.
	// Defaults to 500ms when 0.
	StaggerDelayMs int `yaml:"stagger_delay_ms,omitempty" json:"stagger_delay_ms,omitempty"`
	// CircuitBreaker configures automatic backend suspension on consecutive 429s.
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker,omitempty" json:"circuit_breaker,omitempty"`
}

// CircuitBreakerConfig controls per-backend circuit breaker behavior.
type CircuitBreakerConfig struct {
	// Enabled toggles the circuit breaker system. Default: true.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Threshold is the number of consecutive 429 responses before tripping. Default: 3.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
	// CooldownSec is the number of seconds to keep a tripped backend suspended. Default: 900 (15m).
	CooldownSec int `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
}

type ServerConfig struct {
	Host          string        `yaml:"host,omitempty"`
	Port          int           `yaml:"port,omitempty"`
	Listen        string        `yaml:"listen,omitempty"` // Legacy: host:port. New host/port fields take precedence.
	Domain        string        `yaml:"domain,omitempty"` // Externally-reachable domain for OAuth callbacks and links (e.g. "myserver.tail", "https://example.com").
	APIKeys       []string      `yaml:"api_keys"`
	StatsPath     string        `yaml:"stats_path"`
	DisableStats  bool          `yaml:"disable_stats"`
	ChatDBPath    string        `yaml:"chat_db_path"`
	TitleModel    string        `yaml:"title_model"`
	DefaultModel  string        `yaml:"default_model,omitempty"`
	ModelCacheTTL time.Duration `yaml:"-"` // Set via custom YAML unmarshal; use ModelCacheTTLSec for JSON.

	// WebAuth enables username/password authentication for the web UI.
	// When enabled, all /ui/* routes require a valid session cookie.
	// The proxy API (/v1/*) is unaffected — it continues using API key auth.
	WebAuth bool `yaml:"web_auth"`

	// UsersDBPath is the path to the SQLite database storing web UI users.
	// Defaults to data/users.db when empty.
	UsersDBPath string `yaml:"users_db_path,omitempty"`

	// WebAuthSecret is the HMAC key for signing session cookies.
	// When empty, a random key is generated on startup (sessions invalidated on restart).
	// When set, sessions survive restarts. Must be at least 16 characters.
	WebAuthSecret string `yaml:"web_auth_secret,omitempty"`

	// modelCacheTTLSet tracks whether model_cache_ttl was explicitly provided.
	// When false (field absent), ModelCacheTTL defaults to DefaultModelCacheTTL in Validate().
	// When true and ModelCacheTTL == 0, caching is disabled.
	modelCacheTTLSet bool
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

// ModelConfig specifies a single model with optional metadata overrides.
// It supports both shorthand string form ("gpt-4o") and object form:
//
//   - id: gpt-4o
//     context_length: 128000
//     max_output_tokens: 16384
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
	OAuth        *OAuthConfig      `yaml:"oauth,omitempty"`

	// ModelsURL overrides the URL used for model discovery (/models endpoint).
	// When set, the proxy fetches the model list from this URL instead of
	// base_url + "/models". Useful for backends like OpenCode Go that don't
	// expose their own /models endpoint but share a model catalog with another
	// endpoint (e.g. OpenCode Zen). Chat completions still route to base_url.
	ModelsURL string `yaml:"models_url,omitempty"`

	// DisabledModels lists model IDs that should never be routed through this
	// backend, even if the model is available. The model is excluded from
	// SupportsModel() and ListModels() so it won't appear in /v1/models or
	// the web UI for this backend. Other backends are unaffected.
	DisabledModels []string `yaml:"disabled_models,omitempty"`

	// IdentityProfile overrides the global identity_profile for this backend.
	// When empty, the global setting is used. Set to "none" to explicitly
	// disable spoofing for this backend regardless of the global setting.
	IdentityProfile string `yaml:"identity_profile,omitempty"`

	// CompatMode controls which API compatibility mode to use for backends
	// that support multiple API formats (e.g. Ollama supports openai, anthropic,
	// and native modes). When empty, defaults based on backend type:
	//   - "ollama" → "openai"
	// All other types ignore this field.
	CompatMode string `yaml:"compat_mode,omitempty"`
}

// ModelIDs returns the list of model IDs as plain strings (backward compat).
func (b *BackendConfig) ModelIDs() []string {
	ids := make([]string, len(b.Models))
	for i, m := range b.Models {
		ids[i] = m.ID
	}
	return ids
}

// IsModelDisabled returns true if the given model ID is in the DisabledModels list.
func (b *BackendConfig) IsModelDisabled(modelID string) bool {
	for _, m := range b.DisabledModels {
		if m == modelID {
			return true
		}
	}
	return false
}

// DefaultModelCacheTTL is the default time-to-live for cached model lists.
// When model_cache_ttl is not set or is 0, this default is used.
const DefaultModelCacheTTL = 5 * time.Minute

// UnmarshalYAML implements custom YAML unmarshaling for ServerConfig
// to parse model_cache_ttl as a duration string (e.g. "5m", "300s").
func (sc *ServerConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Use a shadow type to avoid infinite recursion.
	type raw struct {
		Host          string      `yaml:"host,omitempty"`
		Port          int         `yaml:"port,omitempty"`
		Listen        string      `yaml:"listen,omitempty"`
		Domain        string      `yaml:"domain,omitempty"`
		APIKeys       []string    `yaml:"api_keys"`
		StatsPath     string      `yaml:"stats_path"`
		DisableStats  bool        `yaml:"disable_stats"`
		ChatDBPath    string      `yaml:"chat_db_path"`
		TitleModel    string      `yaml:"title_model"`
		DefaultModel  string      `yaml:"default_model,omitempty"`
		ModelCacheTTL interface{} `yaml:"model_cache_ttl,omitempty"`
		WebAuth       bool        `yaml:"web_auth"`
		UsersDBPath   string      `yaml:"users_db_path,omitempty"`
		WebAuthSecret string      `yaml:"web_auth_secret,omitempty"`
	}
	var r raw
	if err := unmarshal(&r); err != nil {
		return err
	}
	sc.Host = r.Host
	sc.Port = r.Port
	sc.Listen = r.Listen
	sc.Domain = r.Domain
	sc.APIKeys = r.APIKeys
	sc.StatsPath = r.StatsPath
	sc.DisableStats = r.DisableStats
	sc.ChatDBPath = r.ChatDBPath
	sc.TitleModel = r.TitleModel
	sc.DefaultModel = r.DefaultModel
	sc.WebAuth = r.WebAuth
	sc.UsersDBPath = r.UsersDBPath
	sc.WebAuthSecret = r.WebAuthSecret

	switch v := r.ModelCacheTTL.(type) {
	case string:
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("server.model_cache_ttl: invalid duration %q: %w", v, err)
		}
		if d < 0 {
			return fmt.Errorf("server.model_cache_ttl: must not be negative, got %s", d)
		}
		sc.ModelCacheTTL = d
		sc.modelCacheTTLSet = true
	case int:
		sc.ModelCacheTTL = time.Duration(v)
		sc.modelCacheTTLSet = true
	case float64:
		sc.ModelCacheTTL = time.Duration(int64(v))
		sc.modelCacheTTLSet = true
	case nil:
		// Not specified — will use default in Validate().
		sc.ModelCacheTTL = 0
		sc.modelCacheTTLSet = false
	default:
		return fmt.Errorf("server.model_cache_ttl: unsupported type %T", v)
	}

	return nil
}

// MarshalYAML implements custom YAML marshaling for ServerConfig
// to serialize model_cache_ttl as a human-readable duration string.
func (sc *ServerConfig) MarshalYAML() (interface{}, error) {
	type raw struct {
		Host          string   `yaml:"host,omitempty"`
		Port          int      `yaml:"port,omitempty"`
		Listen        string   `yaml:"listen,omitempty"`
		Domain        string   `yaml:"domain,omitempty"`
		APIKeys       []string `yaml:"api_keys"`
		StatsPath     string   `yaml:"stats_path"`
		DisableStats  bool     `yaml:"disable_stats"`
		ChatDBPath    string   `yaml:"chat_db_path"`
		TitleModel    string   `yaml:"title_model"`
		DefaultModel  string   `yaml:"default_model,omitempty"`
		ModelCacheTTL string   `yaml:"model_cache_ttl,omitempty"`
		WebAuth       bool     `yaml:"web_auth"`
		UsersDBPath   string   `yaml:"users_db_path,omitempty"`
		WebAuthSecret string   `yaml:"web_auth_secret,omitempty"`
	}

	var ttlStr string
	if sc.modelCacheTTLSet {
		ttlStr = sc.ModelCacheTTL.String()
	}

	return raw{
		Host:          sc.Host,
		Port:          sc.Port,
		Listen:        sc.Listen,
		Domain:        sc.Domain,
		APIKeys:       sc.APIKeys,
		StatsPath:     sc.StatsPath,
		DisableStats:  sc.DisableStats,
		ChatDBPath:    sc.ChatDBPath,
		TitleModel:    sc.TitleModel,
		DefaultModel:  sc.DefaultModel,
		ModelCacheTTL: ttlStr,
		WebAuth:       sc.WebAuth,
		UsersDBPath:   sc.UsersDBPath,
		WebAuthSecret: sc.WebAuthSecret,
	}, nil
}

// UnmarshalYAML implements custom YAML unmarshaling for BackendConfig
// to support both "- model-name" (string) and "- id: model-name" (object)
// in the models list.
func (bc *BackendConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	// Use a shadow type to avoid infinite recursion.
	type raw struct {
		Name            string            `yaml:"name"`
		Type            string            `yaml:"type"`
		BaseURL         string            `yaml:"base_url"`
		APIKey          string            `yaml:"api_key"`
		ExtraHeaders    map[string]string `yaml:"extra_headers,omitempty"`
		ModelsRaw       interface{}       `yaml:"models,omitempty"`
		Enabled         *bool             `yaml:"enabled,omitempty"`
		OAuth           *OAuthConfig      `yaml:"oauth,omitempty"`
		ModelsURL       string            `yaml:"models_url,omitempty"`
		DisabledModels  []string          `yaml:"disabled_models,omitempty"`
		IdentityProfile string            `yaml:"identity_profile,omitempty"`
		CompatMode      string            `yaml:"compat_mode,omitempty"`
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
	bc.OAuth = r.OAuth
	bc.ModelsURL = r.ModelsURL
	bc.DisabledModels = r.DisabledModels
	bc.IdentityProfile = r.IdentityProfile
	bc.CompatMode = r.CompatMode

	if r.ModelsRaw != nil {
		models, err := parseModelsField(r.ModelsRaw)
		if err != nil {
			return fmt.Errorf("backends[%s].models: %w", r.Name, err)
		}
		bc.Models = models
	}
	return nil
}

// MarshalYAML implements custom YAML marshaling for BackendConfig
// to preserve models in their object form and keep custom fields symmetric
// with UnmarshalYAML.
func (bc BackendConfig) MarshalYAML() (interface{}, error) {
	type raw struct {
		Name            string            `yaml:"name"`
		Type            string            `yaml:"type"`
		BaseURL         string            `yaml:"base_url"`
		APIKey          string            `yaml:"api_key"`
		ExtraHeaders    map[string]string `yaml:"extra_headers,omitempty"`
		Models          []ModelConfig     `yaml:"models,omitempty"`
		Enabled         *bool             `yaml:"enabled,omitempty"`
		OAuth           *OAuthConfig      `yaml:"oauth,omitempty"`
		ModelsURL       string            `yaml:"models_url,omitempty"`
		DisabledModels  []string          `yaml:"disabled_models,omitempty"`
		IdentityProfile string            `yaml:"identity_profile,omitempty"`
		CompatMode      string            `yaml:"compat_mode,omitempty"`
	}

	return raw{
		Name:            bc.Name,
		Type:            bc.Type,
		BaseURL:         bc.BaseURL,
		APIKey:          bc.APIKey,
		ExtraHeaders:    bc.ExtraHeaders,
		Models:          bc.Models,
		Enabled:         bc.Enabled,
		OAuth:           bc.OAuth,
		ModelsURL:       bc.ModelsURL,
		DisabledModels:  bc.DisabledModels,
		IdentityProfile: bc.IdentityProfile,
		CompatMode:      bc.CompatMode,
	}, nil
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

// RequiresAPIKey returns true if the backend type requires a static API key.
// OAuth backends and local backends (like Ollama) do not need one.
func (b *BackendConfig) RequiresAPIKey() bool {
	switch b.Type {
	case "copilot", "codex", "ollama":
		return false
	default:
		return true
	}
}

func (c *Config) Validate() error {
	// Resolve listen address from host/port or legacy listen field.
	// Priority: explicit host/port > legacy listen > defaults.
	if c.Server.Host != "" || c.Server.Port != 0 {
		// New fields take precedence.
		host := c.Server.Host
		port := c.Server.Port
		if port == 0 {
			port = 8080
		}
		c.Server.Listen = net.JoinHostPort(host, strconv.Itoa(port))
	} else if c.Server.Listen != "" {
		// Legacy listen field: parse to extract host/port.
		host, portStr, err := net.SplitHostPort(c.Server.Listen)
		if err == nil {
			c.Server.Host = host
			if p, err := strconv.Atoi(portStr); err == nil {
				c.Server.Port = p
			}
		}
	} else {
		// Defaults: bind all interfaces, port 8080.
		c.Server.Host = ""
		c.Server.Port = 8080
		c.Server.Listen = ":8080"
	}
	if c.Server.StatsPath == "" {
		c.Server.StatsPath = filepath.Join("data", "stats.db")
	}
	if c.Server.ChatDBPath == "" {
		c.Server.ChatDBPath = filepath.Join("data", "chat.db")
	}
	if c.Server.UsersDBPath == "" {
		c.Server.UsersDBPath = filepath.Join("data", "users.db")
	}
	// Default model cache TTL: 5 minutes when not explicitly set.
	// Setting model_cache_ttl to "0s" or "0" explicitly disables caching.
	if !c.Server.modelCacheTTLSet {
		c.Server.ModelCacheTTL = DefaultModelCacheTTL
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
		// Some backends (OAuth, local) do not require an api_key.
		if c.Backends[i].RequiresAPIKey() && b.APIKey == "" {
			return fmt.Errorf("backends[%d].api_key: must not be empty for enabled backend", i)
		}
	}
	if err := c.Routing.validate(); err != nil {
		return err
	}
	return nil
}

func (r *RoutingConfig) validate() error {
	validStrategies := map[string]bool{
		"":                 true,
		StrategyPriority:   true,
		StrategyRoundRobin: true,
		StrategyRace:       true,
	}
	if !validStrategies[r.Strategy] {
		return fmt.Errorf("routing.strategy: invalid value %q (must be one of: priority, round-robin, race)", r.Strategy)
	}
	for i, m := range r.Models {
		if !validStrategies[m.Strategy] {
			return fmt.Errorf("routing.models[%d] (%s): invalid strategy %q (must be one of: priority, round-robin, race)", i, m.Model, m.Strategy)
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
	mu          sync.RWMutex
	path        string
	current     *Config
	onChange    []func(*Config)
	selfWriteAt atomic.Int64 // unix-ms timestamp of last programmatic write
	watcher     *fsnotify.Watcher
	done        chan struct{}
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

// markSelfWrite records the current time as the last programmatic write.
// The file watcher uses this to skip reloads triggered by our own writes.
func (m *Manager) markSelfWrite() {
	m.selfWriteAt.Store(time.Now().UnixMilli())
}

// WatchFile starts watching the config file for external changes and auto-reloads.
// It watches the parent directory to handle editors that save via rename (e.g. vim).
// Reloads triggered by the Manager's own write methods are suppressed.
func (m *Manager) WatchFile() error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating file watcher: %w", err)
	}

	dir := filepath.Dir(m.path)
	if err := w.Add(dir); err != nil {
		w.Close()
		return fmt.Errorf("watching directory %s: %w", dir, err)
	}

	m.watcher = w
	m.done = make(chan struct{})

	baseName := filepath.Base(m.path)

	go func() {
		const debounce = 500 * time.Millisecond
		var timer *time.Timer

		for {
			select {
			case <-m.done:
				if timer != nil {
					timer.Stop()
				}
				return
			case event, ok := <-w.Events:
				if !ok {
					return
				}
				if filepath.Base(event.Name) != baseName {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
					continue
				}
				if timer != nil {
					timer.Reset(debounce)
				} else {
					timer = time.AfterFunc(debounce, func() {
						// Skip if we wrote the file ourselves recently.
						if elapsed := time.Since(time.UnixMilli(m.selfWriteAt.Load())); elapsed < time.Second {
							return
						}
						log.Info().Msg("config file changed externally, reloading...")
						if err := m.Reload(); err != nil {
							log.Error().Err(err).Msg("config reload failed")
						} else {
							log.Info().Msg("config reloaded successfully")
						}
					})
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Error().Err(err).Msg("config file watcher error")
			}
		}
	}()

	return nil
}

// Close stops the file watcher if running. Safe to call when not watching.
func (m *Manager) Close() {
	if m.done != nil {
		close(m.done)
	}
	if m.watcher != nil {
		m.watcher.Close()
	}
}

// SaveRaw writes raw YAML bytes to the config file after validating them.
func (m *Manager) SaveRaw(data []byte) error {
	if _, err := Parse(data); err != nil {
		return err
	}
	m.markSelfWrite()
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
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// SwitchBackendType changes a backend's type between "openai" and "anthropic",
// or switches an ollama backend's compat_mode between "openai", "anthropic", and "native".
// Updates base URL and API key, persists the file, and reloads.
func (m *Manager) SwitchBackendType(name, newType, baseURL, apiKey string) error {
	m.mu.Lock()
	found := false
	for i, b := range m.current.Backends {
		if b.Name == name {
			if b.Type == "ollama" {
				// For ollama backends, switch compat_mode instead of type.
				switch newType {
				case "openai", "anthropic", "native":
					m.current.Backends[i].CompatMode = newType
				default:
					m.mu.Unlock()
					return fmt.Errorf("unsupported ollama compat_mode %q; use openai, anthropic, or native", newType)
				}
			} else {
				// For non-ollama backends, switch type between openai and anthropic.
				switch newType {
				case "openai", "anthropic":
					// ok
				default:
					m.mu.Unlock()
					return fmt.Errorf("unsupported target type %q; only openai and anthropic are allowed", newType)
				}
				switch b.Type {
				case "openai", "anthropic":
					// ok — allowed to switch
				default:
					m.mu.Unlock()
					return fmt.Errorf("cannot switch backend %q of type %q; only openai and anthropic backends can be switched", name, b.Type)
				}
				m.current.Backends[i].Type = newType
			}
			if baseURL != "" {
				m.current.Backends[i].BaseURL = baseURL
			}
			if apiKey != "" {
				m.current.Backends[i].APIKey = apiKey
			}
			// Clear OAuth — switching to a non-OAuth type.
			m.current.Backends[i].OAuth = nil
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
	m.markSelfWrite()
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
	m.markSelfWrite()
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
	m.markSelfWrite()
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
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// ToggleDisabledModel adds or removes a model from a backend's disabled_models list,
// persists the file, and reloads. When disabled is true the model is added; when false
// it is removed. Returns an error if the backend is not found.
func (m *Manager) ToggleDisabledModel(backendName, modelID string, disabled bool) error {
	m.mu.Lock()
	found := false
	for i, b := range m.current.Backends {
		if b.Name == backendName {
			if disabled {
				// Add to disabled list if not already present.
				for _, d := range b.DisabledModels {
					if d == modelID {
						found = true
						break
					}
				}
				if !found {
					m.current.Backends[i].DisabledModels = append(m.current.Backends[i].DisabledModels, modelID)
				}
			} else {
				// Remove from disabled list.
				filtered := make([]string, 0, len(b.DisabledModels))
				for _, d := range b.DisabledModels {
					if d != modelID {
						filtered = append(filtered, d)
					}
				}
				m.current.Backends[i].DisabledModels = filtered
			}
			found = true
			break
		}
	}
	cfg := m.current
	m.mu.Unlock()

	if !found {
		return fmt.Errorf("backend %q not found", backendName)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// ReplaceDisabledModels replaces the entire disabled_models list for a backend.
// Pass an empty slice to enable all models. Persists the config file and reloads.
func (m *Manager) ReplaceDisabledModels(backendName string, models []string) error {
	m.mu.Lock()
	found := false
	for i, b := range m.current.Backends {
		if b.Name == backendName {
			m.current.Backends[i].DisabledModels = models
			found = true
			break
		}
	}
	cfg := m.current
	m.mu.Unlock()

	if !found {
		return fmt.Errorf("backend %q not found", backendName)
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// UpdateTitleModel sets the server.title_model field, persists the file, and reloads.
func (m *Manager) UpdateTitleModel(titleModel string) error {
	m.mu.Lock()
	m.current.Server.TitleModel = titleModel
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// UpdateDefaultModel sets the server.default_model field, persists the file, and reloads.
func (m *Manager) UpdateDefaultModel(defaultModel string) error {
	m.mu.Lock()
	m.current.Server.DefaultModel = defaultModel
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// UpdateModelCacheTTL updates the model cache TTL, persists the config, and
// reloads. A TTL of 0 disables caching. Backends are recreated on reload so
// the new TTL takes effect immediately.
func (m *Manager) UpdateModelCacheTTL(ttl time.Duration) error {
	m.mu.Lock()
	m.current.Server.ModelCacheTTL = ttl
	m.current.Server.modelCacheTTLSet = true
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// AddBackend appends a new backend to the configuration, persists the file, and
// reloads. It validates that the name is unique and the type is one of the
// supported backend types. Default values are applied for well-known types.
func (m *Manager) AddBackend(bc BackendConfig) error {
	// Validate type.
	switch bc.Type {
	case "openai", "anthropic", "copilot", "codex", "ollama", "":
		// ok
	default:
		return fmt.Errorf("unsupported backend type %q", bc.Type)
	}
	if bc.Type == "" {
		bc.Type = "openai"
	}

	// Validate name.
	if bc.Name == "" {
		return fmt.Errorf("backend name is required")
	}

	m.mu.Lock()
	for _, existing := range m.current.Backends {
		if existing.Name == bc.Name {
			m.mu.Unlock()
			return fmt.Errorf("backend %q already exists", bc.Name)
		}
	}
	m.current.Backends = append(m.current.Backends, bc)
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// DeleteBackend removes a backend by name from the configuration, persists
// the file, and reloads. It validates that at least one enabled backend remains.
func (m *Manager) DeleteBackend(name string) error {
	m.mu.Lock()

	idx := -1
	for i, b := range m.current.Backends {
		if b.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return fmt.Errorf("backend %q not found", name)
	}

	// Check that at least one other enabled backend would remain.
	remaining := slices.Clone(m.current.Backends)
	remaining = slices.Delete(remaining, idx, idx+1)
	enabledCount := 0
	for _, b := range remaining {
		if b.IsEnabled() {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		m.mu.Unlock()
		return fmt.Errorf("cannot delete the last enabled backend")
	}

	m.current.Backends = remaining
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// UpdateServerAddr updates the host and port fields, clears the legacy listen
// field, persists the file, and reloads. The caller should note that a server
// restart is required for the new address to take effect.
func (m *Manager) UpdateServerAddr(host string, port int) error {
	m.mu.Lock()
	m.current.Server.Host = host
	m.current.Server.Port = port
	m.current.Server.Listen = "" // clear legacy field — host/port take precedence
	cfg := m.current
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return m.Reload()
}

// SetGlobalIdentityProfile sets the global identity_profile, persists the file,
// and reloads configuration.
func (m *Manager) SetGlobalIdentityProfile(profileID string) error {
	m.mu.Lock()
	previousProfile := m.current.IdentityProfile
	m.current.IdentityProfile = profileID
	cfg := m.current
	m.mu.Unlock()

	log.Debug().
		Str("path", m.path).
		Str("previous_profile", previousProfile).
		Str("new_profile", profileID).
		Msg("persisting global identity profile change")

	data, err := yaml.Marshal(cfg)
	if err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to marshal config for global identity profile change")
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to write config for global identity profile change")
		return fmt.Errorf("writing config: %w", err)
	}
	log.Info().
		Str("path", m.path).
		Str("previous_profile", previousProfile).
		Str("new_profile", profileID).
		Msg("wrote global identity profile change to config")
	if err := m.Reload(); err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to reload config after global identity profile change")
		return err
	}
	reloadedProfile := ""
	if cfg := m.Get(); cfg != nil {
		reloadedProfile = cfg.IdentityProfile
	}
	log.Info().
		Str("path", m.path).
		Str("requested_profile", profileID).
		Str("reloaded_profile", reloadedProfile).
		Msg("reloaded config after global identity profile change")
	return nil
}

// SetBackendIdentityProfile sets the identity_profile for a specific backend,
// persists the file, and reloads configuration.
func (m *Manager) SetBackendIdentityProfile(backendName, profileID string) error {
	m.mu.Lock()
	found := false
	previousProfile := ""
	for i, b := range m.current.Backends {
		if b.Name == backendName {
			previousProfile = m.current.Backends[i].IdentityProfile
			m.current.Backends[i].IdentityProfile = profileID
			found = true
			break
		}
	}
	cfg := m.current
	m.mu.Unlock()

	if !found {
		log.Warn().
			Str("path", m.path).
			Str("backend", backendName).
			Str("new_profile", profileID).
			Msg("backend not found for identity profile change")
		return fmt.Errorf("backend %q not found", backendName)
	}

	log.Debug().
		Str("path", m.path).
		Str("backend", backendName).
		Str("previous_profile", previousProfile).
		Str("new_profile", profileID).
		Msg("persisting backend identity profile change")

	data, err := yaml.Marshal(cfg)
	if err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("backend", backendName).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to marshal config for backend identity profile change")
		return fmt.Errorf("marshaling config: %w", err)
	}
	m.markSelfWrite()
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("backend", backendName).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to write config for backend identity profile change")
		return fmt.Errorf("writing config: %w", err)
	}
	log.Info().
		Str("path", m.path).
		Str("backend", backendName).
		Str("previous_profile", previousProfile).
		Str("new_profile", profileID).
		Msg("wrote backend identity profile change to config")
	if err := m.Reload(); err != nil {
		log.Error().
			Err(err).
			Str("path", m.path).
			Str("backend", backendName).
			Str("previous_profile", previousProfile).
			Str("new_profile", profileID).
			Msg("failed to reload config after backend identity profile change")
		return err
	}
	reloadedProfile := ""
	if cfg := m.Get(); cfg != nil {
		for _, b := range cfg.Backends {
			if b.Name == backendName {
				reloadedProfile = b.IdentityProfile
				break
			}
		}
	}
	log.Info().
		Str("path", m.path).
		Str("backend", backendName).
		Str("requested_profile", profileID).
		Str("reloaded_profile", reloadedProfile).
		Msg("reloaded config after backend identity profile change")
	return nil
}
