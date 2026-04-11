// Package oauth provides token storage and management for OAuth-based LLM backends.
package oauth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// defaultExpiryMargin is the safety margin applied when checking token expiry
	// to account for clock skew between the proxy and the token issuer.
	defaultExpiryMargin = 30 * time.Second
)

// TokenData represents a persisted OAuth token with metadata.
type TokenData struct {
	AccessToken  string    `json:"access_token"`
	TokenType    string    `json:"token_type,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshIn    int       `json:"refresh_in,omitempty"` // seconds until proactive refresh
	ObtainedAt   time.Time `json:"obtained_at"`
	Source       string    `json:"source,omitempty"` // e.g., "env:GH_TOKEN", "gh_cli", "hosts.yml"
	GitHubToken  string    `json:"github_token,omitempty"` // stored GitHub token for Copilot re-exchange
}

// IsExpired returns true if the token has expired, applying a safety margin
// for clock skew (tokens that expire within 30 seconds are treated as expired).
func (t *TokenData) IsExpired() bool {
	if t == nil {
		return true
	}
	return time.Now().After(t.ExpiresAt.Add(-defaultExpiryMargin))
}

// NeedsRefresh returns true if the token should be proactively refreshed,
// based on the refresh_in field or a default of 80% of the token's lifetime.
func (t *TokenData) NeedsRefresh() bool {
	if t == nil {
		return true
	}
	if t.RefreshIn > 0 {
		refreshAt := t.ObtainedAt.Add(time.Duration(t.RefreshIn) * time.Second)
		return time.Now().After(refreshAt.Add(-defaultExpiryMargin))
	}
	// Default: refresh at 80% of the token's TTL
	ttl := t.ExpiresAt.Sub(t.ObtainedAt)
	if ttl <= 0 {
		return true
	}
	refreshAt := t.ObtainedAt.Add(time.Duration(float64(ttl) * 0.8))
	return time.Now().After(refreshAt)
}

// TokenStore provides thread-safe token storage with JSON file persistence
// and in-memory caching.
type TokenStore struct {
	mu       sync.RWMutex
	filePath string
	token    *TokenData

	// refreshMu serializes refresh attempts so only one refresh runs at a time.
	refreshMu  sync.Mutex
	refreshing bool
}

// NewTokenStore creates a new TokenStore that persists tokens to the given file path.
// If the file exists, tokens are loaded from disk. If the parent directory does not
// exist, it is created with 0700 permissions. If the file is corrupted, a warning
// is logged and the store starts fresh.
func NewTokenStore(filePath string) (*TokenStore, error) {
	ts := &TokenStore{
		filePath: filePath,
	}

	// Create parent directory if it doesn't exist.
	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating token directory %s: %w", dir, err)
	}

	// Attempt to load existing token file.
	if err := ts.loadFromDisk(); err != nil {
		if !os.IsNotExist(err) {
			log.Printf("warning: failed to load token file %s: %v; starting fresh", filePath, err)
		}
	}

	return ts, nil
}

// loadFromDisk reads the token file from disk into memory.
func (ts *TokenStore) loadFromDisk() error {
	data, err := os.ReadFile(ts.filePath)
	if err != nil {
		return err
	}

	var token TokenData
	if err := json.Unmarshal(data, &token); err != nil {
		return fmt.Errorf("parsing token file: %w", err)
	}

	ts.mu.Lock()
	ts.token = &token
	ts.mu.Unlock()

	return nil
}

// Get returns the current in-memory token, or nil if no token is available.
// This is a fast operation that does not hit disk.
func (ts *TokenStore) Get() *TokenData {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.token
}

// ValidToken returns the current token only if it is not expired.
// Returns nil if no token is available or the token has expired.
func (ts *TokenStore) ValidToken() *TokenData {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	if ts.token == nil || ts.token.IsExpired() {
		return nil
	}
	return ts.token
}

// Save persists a token to both the in-memory cache and disk.
// The write is atomic (write to temp file, then rename).
// The token file is created with 0600 permissions.
func (ts *TokenStore) Save(token *TokenData) error {
	ts.mu.Lock()
	ts.token = token
	ts.mu.Unlock()

	return ts.persistToDisk()
}

// persistToDisk writes the current token to disk atomically.
func (ts *TokenStore) persistToDisk() error {
	ts.mu.RLock()
	token := ts.token
	ts.mu.RUnlock()

	if token == nil {
		return nil
	}

	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling token: %w", err)
	}

	// Write to a temp file in the same directory, then rename for atomicity.
	dir := filepath.Dir(ts.filePath)
	tmpFile, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}

	// Set permissions before closing to ensure 0600.
	if err := tmpFile.Chmod(0600); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("setting permissions on temp file: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}

	// Atomic rename.
	if err := os.Rename(tmpPath, ts.filePath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}

	return nil
}

// Clear removes the token from both memory and disk.
func (ts *TokenStore) Clear() error {
	ts.mu.Lock()
	ts.token = nil
	ts.mu.Unlock()

	err := os.Remove(ts.filePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing token file: %w", err)
	}
	return nil
}

// RefreshCoordination serializes token refresh attempts. It returns a function
// that must be called to signal completion. If a refresh is already in progress,
// the second call returns (nil, nil, nil) — the caller should use the existing
// cached token. The first return value is a "still valid" flag: if true, the
// token is still usable (not expired) and the caller can proceed with it.
// The second return value is a function to call when refresh completes (or nil).
//
// Usage:
//
//	stillValid, done, err := ts.StartRefresh()
//	if err != nil { ... }
//	if done == nil {
//	    // Another refresh is in progress; use cached token
//	    return ts.ValidToken()
//	}
//	defer done()
//	// ... do the actual refresh ...
//	ts.Save(newToken)
func (ts *TokenStore) StartRefresh() (stillValid bool, done func(), err error) {
	ts.refreshMu.Lock()

	// Check if another refresh is already running.
	if ts.refreshing {
		ts.refreshMu.Unlock()
		// Return the current cached token state.
		ts.mu.RLock()
		t := ts.token
		ts.mu.RUnlock()
		valid := t != nil && !t.IsExpired()
		return valid, nil, nil
	}

	ts.refreshing = true
	ts.refreshMu.Unlock()

	// Check current token state.
	ts.mu.RLock()
	t := ts.token
	ts.mu.RUnlock()
	valid := t != nil && !t.IsExpired()

	return valid, func() {
		ts.refreshMu.Lock()
		ts.refreshing = false
		ts.refreshMu.Unlock()
	}, nil
}

// SetRefreshError records that a refresh failed. Used for tracking transient
// failure state. The token store continues serving stale (but unexpired) tokens.
// This is a no-op for now but can be extended for retry/backoff tracking.
func (ts *TokenStore) SetRefreshError(err error) {
	if err != nil {
		log.Printf("token refresh error for %s: %v", ts.filePath, err)
	}
}
