// Package circuit implements per-backend circuit breakers that trip on
// consecutive failure responses (e.g. 429, 403), temporarily removing
// the backend from routing until a cooldown expires.
package circuit

import (
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// State represents the state of a circuit breaker.
type State int

const (
	Closed   State = iota // Healthy — backend accepts requests.
	Open                  // Tripped — backend is rate-limited, skip it.
	HalfOpen              // Probing — allow one request to test recovery.
)

func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// BreakerState is a snapshot of a breaker's state for UI display.
type BreakerState struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Failures   int       `json:"failures"`
	Threshold  int       `json:"threshold"`
	TrippedAt  time.Time `json:"tripped_at,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
	Cooldown   string    `json:"cooldown"`
	Reason     string    `json:"reason,omitempty"`
}

// Config holds circuit breaker configuration.
type Config struct {
	Enabled   bool `yaml:"enabled" json:"enabled"`
	Threshold int  `yaml:"threshold" json:"threshold"`
	Cooldown  int  `yaml:"cooldown" json:"cooldown"` // Seconds.
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:   true,
		Threshold: 3,
		Cooldown:  300,
	}
}

// breaker is a single backend's circuit breaker.
type breaker struct {
	mu         sync.Mutex
	name       string
	state      State
	failures   int // Consecutive failure count.
	threshold  int // Trip after this many consecutive 429s.
	cooldown   time.Duration
	trippedAt  time.Time
	retryAfter time.Time // When to transition from Open to HalfOpen.
	reason     string
}

func (b *breaker) recordFailure(retryAfterHint time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	log.Debug().Str("backend", b.name).Int("failures", b.failures).Int("threshold", b.threshold).Msg("circuit: failure recorded")

	if b.failures >= b.threshold && b.state != Open {
		b.state = Open
		b.trippedAt = time.Now()

		cooldown := b.cooldown
		if retryAfterHint > 0 && retryAfterHint < 2*time.Hour {
			cooldown = retryAfterHint
		}
		b.retryAfter = time.Now().Add(cooldown)
		b.reason = fmt.Sprintf("consecutive failures (%d)", b.failures)

		log.Warn().
			Str("backend", b.name).
			Int("failures", b.failures).
			Str("cooldown", cooldown.String()).
			Str("retry_after", b.retryAfter.Format(time.RFC3339)).
			Msg("circuit: breaker tripped")
	}
}

func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == HalfOpen {
		log.Info().Str("backend", b.name).Msg("circuit: probe succeeded, closing breaker")
	}
	b.failures = 0
	b.state = Closed
	b.reason = ""
}

// isOpen returns true if the backend should be skipped.
// Transitions Open to HalfOpen when cooldown expires.
func (b *breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open && time.Now().After(b.retryAfter) {
		b.state = HalfOpen
		log.Info().Str("backend", b.name).Msg("circuit: cooldown expired, transitioning to half-open")
	}

	return b.state == Open
}

func (b *breaker) snapshot() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == Open && time.Now().After(b.retryAfter) {
		b.state = HalfOpen
	}

	s := BreakerState{
		Name:      b.name,
		State:     b.state.String(),
		Failures:  b.failures,
		Threshold: b.threshold,
		Cooldown:  b.cooldown.String(),
		Reason:    b.reason,
	}
	if !b.trippedAt.IsZero() {
		s.TrippedAt = b.trippedAt
	}
	if !b.retryAfter.IsZero() {
		s.RetryAfter = b.retryAfter
	}
	return s
}

func (b *breaker) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	log.Info().Str("backend", b.name).Msg("circuit: manual reset")
	b.failures = 0
	b.state = Closed
	b.trippedAt = time.Time{}
	b.retryAfter = time.Time{}
	b.reason = ""
}

func (b *breaker) updateConfig(threshold int, cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.threshold = threshold
	b.cooldown = cooldown
}

// Manager manages per-backend circuit breakers.
type Manager struct {
	mu       sync.RWMutex
	breakers map[string]*breaker
	config   Config
}

// NewManager creates a circuit breaker manager with the given config.
func NewManager(cfg Config) *Manager {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 300
	}
	return &Manager{
		breakers: make(map[string]*breaker),
		config:   cfg,
	}
}

// RecordFailure records a failure response for the named backend.
func (m *Manager) RecordFailure(backendName string, retryAfterHint time.Duration) {
	if !m.config.Enabled {
		return
	}
	b := m.getOrCreate(backendName)
	b.recordFailure(retryAfterHint)
}

// RecordSuccess records a successful response for the named backend.
func (m *Manager) RecordSuccess(backendName string) {
	if !m.config.Enabled {
		return
	}
	b := m.getOrCreate(backendName)
	b.recordSuccess()
}

// IsOpen returns true if the backend should be skipped.
func (m *Manager) IsOpen(backendName string) bool {
	if !m.config.Enabled {
		return false
	}
	m.mu.RLock()
	b, ok := m.breakers[backendName]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	return b.isOpen()
}

// AllStates returns snapshots of all breaker states.
func (m *Manager) AllStates() []BreakerState {
	m.mu.RLock()
	defer m.mu.RUnlock()

	states := make([]BreakerState, 0, len(m.breakers))
	for _, b := range m.breakers {
		states = append(states, b.snapshot())
	}
	return states
}

// State returns the state of a specific backend's breaker.
func (m *Manager) State(backendName string) BreakerState {
	m.mu.RLock()
	b, ok := m.breakers[backendName]
	m.mu.RUnlock()
	if !ok {
		return BreakerState{
			Name:      backendName,
			State:     Closed.String(),
			Threshold: m.config.Threshold,
			Cooldown:  time.Duration(m.config.Cooldown * int(time.Second)).String(),
		}
	}
	return b.snapshot()
}

// Reset manually resets a tripped breaker.
func (m *Manager) Reset(backendName string) {
	b := m.getOrCreate(backendName)
	b.reset()
}

// ResetAll resets all breakers.
func (m *Manager) ResetAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, b := range m.breakers {
		b.reset()
	}
}

// UpdateConfig updates the configuration for all breakers.
func (m *Manager) UpdateConfig(cfg Config) {
	if cfg.Threshold <= 0 {
		cfg.Threshold = 3
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 300
	}
	cooldown := time.Duration(cfg.Cooldown) * time.Second

	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
	for _, b := range m.breakers {
		b.updateConfig(cfg.Threshold, cooldown)
	}
	log.Info().
		Bool("enabled", cfg.Enabled).
		Int("threshold", cfg.Threshold).
		Int("cooldown_sec", cfg.Cooldown).
		Msg("circuit: config updated")
}

// Enabled returns whether the circuit breaker system is active.
func (m *Manager) Enabled() bool {
	return m.config.Enabled
}

// GetConfig returns the current config.
func (m *Manager) GetConfig() Config {
	return m.config
}

// FilterEntries filters out backends with open circuit breakers.
// If all entries would be filtered, returns the original slice (never blocks everything).
func (m *Manager) FilterEntries(names []string) []string {
	if !m.config.Enabled {
		return names
	}

	var active []string
	for _, name := range names {
		if !m.IsOpen(name) {
			active = append(active, name)
		}
	}

	if len(active) == 0 {
		log.Warn().Msg("circuit: all backends tripped, ignoring breakers")
		return names
	}

	if len(active) < len(names) {
		log.Debug().Int("total", len(names)).Int("active", len(active)).Msg("circuit: filtered backends")
	}
	return active
}

// EnsureBackend ensures a breaker exists for a backend name.
func (m *Manager) EnsureBackend(name string) {
	m.getOrCreate(name)
}

func (m *Manager) getOrCreate(name string) *breaker {
	m.mu.RLock()
	b, ok := m.breakers[name]
	m.mu.RUnlock()
	if ok {
		return b
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.breakers[name]; ok {
		return b
	}

	cooldown := time.Duration(m.config.Cooldown) * time.Second
	b = &breaker{
		name:      name,
		state:     Closed,
		threshold: m.config.Threshold,
		cooldown:  cooldown,
	}
	m.breakers[name] = b
	return b
}

// ParseRetryAfter parses a Retry-After header value (seconds or HTTP-date).
// Returns 0 if unparseable.
func ParseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	var secs int
	if _, err := fmt.Sscanf(value, "%d", &secs); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := time.Parse(time.RFC1123, value); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}
