package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- VAL-TOKEN-009: Copilot token exchange success ---

func TestExchangeCopilotToken_Success(t *testing.T) {
	// Set up mock GitHub API server
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	refreshIn := 1500 // 25 minutes in seconds
	mockToken := "tid=abc123;fcv1=1:mac"

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request path
		if r.URL.Path != "/copilot_internal/v2/token" {
			t.Errorf("request path = %q, want /copilot_internal/v2/token", r.URL.Path)
		}

		// Verify request method
		if r.Method != http.MethodGet {
			t.Errorf("request method = %q, want GET", r.Method)
		}

		// Verify Authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-github-token" {
			t.Errorf("Authorization header = %q, want %q", auth, "Bearer test-github-token")
		}

		// Verify required Copilot headers
		if r.Header.Get("Editor-Version") == "" {
			t.Error("Editor-Version header is missing")
		}
		if r.Header.Get("Editor-Plugin-Version") == "" {
			t.Error("Editor-Plugin-Version header is missing")
		}

		resp := copilotTokenResponse{
			ExpiresAt: expiresAt,
			RefreshIn: refreshIn,
			Token:     mockToken,
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	token, err := exchanger.Exchange(context.Background(), "test-github-token")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if token.AccessToken != mockToken {
		t.Errorf("access token = %q, want %q", token.AccessToken, mockToken)
	}
	if token.RefreshIn != refreshIn {
		t.Errorf("refresh_in = %d, want %d", token.RefreshIn, refreshIn)
	}
	if token.Source != "copilot_exchange" {
		t.Errorf("source = %q, want %q", token.Source, "copilot_exchange")
	}
	if token.IsExpired() {
		t.Error("token should not be expired")
	}

	// Verify token was persisted to store
	stored := ts.Get()
	if stored == nil {
		t.Fatal("token should be persisted in store")
	}
	if stored.AccessToken != mockToken {
		t.Errorf("stored access token = %q, want %q", stored.AccessToken, mockToken)
	}
}

// --- VAL-TOKEN-010: Copilot token exchange with invalid GitHub token ---

func TestExchangeCopilotToken_InvalidGitHubToken(t *testing.T) {
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	_, err = exchanger.Exchange(context.Background(), "invalid-token")
	if err == nil {
		t.Fatal("Exchange should fail with invalid token")
	}

	// Error should mention authentication
	if !contains(err.Error(), "401") && !contains(err.Error(), "unauthorized") {
		t.Errorf("error should mention 401 or unauthorized: %v", err)
	}
}

func TestExchangeCopilotToken_ForbiddenToken(t *testing.T) {
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"Forbidden"}`))
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	_, err = exchanger.Exchange(context.Background(), "forbidden-token")
	if err == nil {
		t.Fatal("Exchange should fail with forbidden token")
	}
}

// --- VAL-TOKEN-011: Copilot token exchange with GitHub API rate limit ---

func TestExchangeCopilotToken_RateLimit(t *testing.T) {
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	_, err = exchanger.Exchange(context.Background(), "test-token")
	if err == nil {
		t.Fatal("Exchange should fail with rate limit")
	}

	if !contains(err.Error(), "429") && !contains(err.Error(), "rate limit") {
		t.Errorf("error should mention 429 or rate limit: %v", err)
	}
}

// --- VAL-TOKEN-012: Copilot token exchange network failure ---

func TestExchangeCopilotToken_NetworkFailure(t *testing.T) {
	// Create a server that's immediately closed to simulate network failure
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never reached
	}))
	server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	_, err = exchanger.Exchange(context.Background(), "test-token")
	if err == nil {
		t.Fatal("Exchange should fail with network error")
	}
}

// --- VAL-TOKEN-019: Proactive token refresh based on refresh_in field ---

func TestExchangeCopilotToken_RefreshInFieldUsed(t *testing.T) {
	// Token obtained 28 minutes ago, refresh_in = 1500 seconds (25 min)
	// → Should trigger proactive refresh
	now := time.Now()

	refreshIn := 1500

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := copilotTokenResponse{
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			RefreshIn: 1500,
			Token:     "refreshed-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Set a token that needs refresh (past refresh_in)
	oldToken := &TokenData{
		AccessToken: "old-token",
		ExpiresAt:   now.Add(2 * time.Minute),
		ObtainedAt:  now.Add(-28 * time.Minute),
		RefreshIn:   refreshIn,
		Source:      "copilot_exchange",
	}
	if err := ts.Save(oldToken); err != nil {
		t.Fatalf("Save: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	token, err := exchanger.GetOrRefresh(context.Background(), "test-github-token")
	if err != nil {
		t.Fatalf("GetOrRefresh: %v", err)
	}

	if token.AccessToken != "refreshed-token" {
		t.Errorf("access token = %q, want %q (should have refreshed)", token.AccessToken, "refreshed-token")
	}
}

// --- VAL-TOKEN-034: Default refresh interval when refresh_in is missing ---

func TestExchangeCopilotToken_MissingRefreshIn(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	// Response does NOT include refresh_in

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate response without refresh_in by using a map
		resp := map[string]interface{}{
			"expires_at": expiresAt,
			"token":      "no-refresh-in-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	token, err := exchanger.Exchange(context.Background(), "test-github-token")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	if token.AccessToken != "no-refresh-in-token" {
		t.Errorf("access token = %q, want %q", token.AccessToken, "no-refresh-in-token")
	}

	// Token should still be parseable even without refresh_in
	if token.IsExpired() {
		t.Error("token should not be expired")
	}

	// NeedsRefresh should use default 80% of TTL
	// Token was just obtained, so it should NOT need refresh yet
	if token.NeedsRefresh() {
		t.Error("freshly obtained token should not need refresh")
	}
}

// --- VAL-TOKEN-035: Proxy startup with no network (uses cached token) ---

func TestExchangeCopilotToken_OfflineStartup_WithCache(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Pre-populate with a valid cached token
	cachedToken := &TokenData{
		AccessToken: "cached-offline-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(25 * time.Minute),
		ObtainedAt:  time.Now().Add(-5 * time.Minute),
		RefreshIn:   1500,
		Source:      "copilot_exchange",
	}
	if err := ts.Save(cachedToken); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Create exchanger with a broken URL (simulates no network)
	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL("http://127.0.0.1:0"))

	// GetOrRefresh should return the cached token without attempting exchange
	token, err := exchanger.GetOrRefresh(context.Background(), "test-github-token")
	if err != nil {
		t.Fatalf("GetOrRefresh with cached token and no network: %v", err)
	}

	if token.AccessToken != "cached-offline-token" {
		t.Errorf("access token = %q, want %q", token.AccessToken, "cached-offline-token")
	}
}

func TestExchangeCopilotToken_OfflineStartup_NoCache(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "copilot.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// No cached token, and network is down
	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL("http://127.0.0.1:0"))

	_, err = exchanger.GetOrRefresh(context.Background(), "test-github-token")
	if err == nil {
		t.Fatal("GetOrRefresh should fail with no cache and no network")
	}
}

// --- Token exchange response parsing ---

func TestExchangeCopilotToken_ResponseParsing(t *testing.T) {
	expiresAt := time.Now().Add(30 * time.Minute).Unix()

	tests := []struct {
		name      string
		response  copilotTokenResponse
		wantToken string
	}{
		{
			name: "standard response",
			response: copilotTokenResponse{
				ExpiresAt: expiresAt,
				RefreshIn: 1800,
				Token:     "tid=std123;fcv1=1:mac",
			},
			wantToken: "tid=std123;fcv1=1:mac",
		},
		{
			name: "long token",
			response: copilotTokenResponse{
				ExpiresAt: expiresAt,
				RefreshIn: 900,
				Token:     "tid=verylongtokenvalue1234567890abcdef;fcv1=2;sku=monthly:mac",
			},
			wantToken: "tid=verylongtokenvalue1234567890abcdef;fcv1=2;sku=monthly:mac",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(tt.response)
			}))
			defer server.Close()

			dir, cleanup := helperTempDir(t)
			defer cleanup()

			ts, err := NewTokenStore(filepath.Join(dir, "parse-test.json"))
			if err != nil {
				t.Fatalf("NewTokenStore: %v", err)
			}

			exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))
			token, err := exchanger.Exchange(context.Background(), "test-token")
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}

			if token.AccessToken != tt.wantToken {
				t.Errorf("access token = %q, want %q", token.AccessToken, tt.wantToken)
			}
			if token.ExpiresAt.IsZero() {
				t.Error("expires_at should not be zero")
			}
			if token.ObtainedAt.IsZero() {
				t.Error("obtained_at should not be zero")
			}
		})
	}
}

// --- GetOrRefresh returns cached valid token without exchange ---

func TestExchangeCopilotToken_CachedTokenReturned(t *testing.T) {
	requestCount := 0
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		resp := copilotTokenResponse{
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			RefreshIn: 1800,
			Token:     "fresh-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "cached.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Set a valid, fresh token
	freshToken := &TokenData{
		AccessToken: "fresh-cached",
		ExpiresAt:   time.Now().Add(25 * time.Minute),
		ObtainedAt:  time.Now().Add(-5 * time.Minute),
		RefreshIn:   1800,
		Source:      "copilot_exchange",
	}
	if err := ts.Save(freshToken); err != nil {
		t.Fatalf("Save: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	// Should return cached token without making HTTP request
	token, err := exchanger.GetOrRefresh(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("GetOrRefresh: %v", err)
	}

	if token.AccessToken != "fresh-cached" {
		t.Errorf("access token = %q, want %q (cached)", token.AccessToken, "fresh-cached")
	}

	if requestCount != 0 {
		t.Errorf("expected 0 HTTP requests, got %d (should use cache)", requestCount)
	}
}

// --- Concurrent GetOrRefresh ---

func TestExchangeCopilotToken_ConcurrentGetOrRefresh(t *testing.T) {
	requestCount := 0
	var mu sync.Mutex

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()

		// Simulate slow response
		time.Sleep(100 * time.Millisecond)

		resp := copilotTokenResponse{
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			RefreshIn: 1800,
			Token:     "concurrent-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "concurrent.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	// Set expired token so all goroutines need to refresh
	expiredToken := helperExpiredToken("expired")
	if err := ts.Save(expiredToken); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const numGoroutines = 10
	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			token, err := exchanger.GetOrRefresh(context.Background(), "test-token")
			if err != nil {
				errors <- fmt.Errorf("goroutine %d: %v", id, err)
				return
			}
			if token.AccessToken != "concurrent-token" {
				errors <- fmt.Errorf("goroutine %d: access token = %q, want %q",
					id, token.AccessToken, "concurrent-token")
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	// Should have made exactly 1 exchange request (serialized refresh)
	mu.Lock()
	count := requestCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected exactly 1 HTTP request due to serialized refresh, got %d", count)
	}
}

// --- No GitHub token provided ---

func TestExchangeCopilotToken_EmptyGitHubToken(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "empty.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts)

	_, err = exchanger.Exchange(context.Background(), "")
	if err == nil {
		t.Fatal("Exchange should fail with empty GitHub token")
	}
}

// --- Token file persisted after exchange ---

func TestExchangeCopilotToken_PersistsToFile(t *testing.T) {
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := copilotTokenResponse{
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			RefreshIn: 1800,
			Token:     "persisted-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "persist-test.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))
	_, err = exchanger.Exchange(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		t.Fatal("token file should exist after exchange")
	}

	// Load into a new store to verify round-trip
	ts2, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore (second): %v", err)
	}

	got := ts2.Get()
	if got == nil {
		t.Fatal("loaded token should not be nil")
	}
	if got.AccessToken != "persisted-token" {
		t.Errorf("loaded access token = %q, want %q", got.AccessToken, "persisted-token")
	}
	if got.Source != "copilot_exchange" {
		t.Errorf("source = %q, want %q", got.Source, "copilot_exchange")
	}
}

// --- Context cancellation ---

func TestExchangeCopilotToken_ContextCancelled(t *testing.T) {
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second) // Slow response
		resp := copilotTokenResponse{
			ExpiresAt: time.Now().Add(30 * time.Minute).Unix(),
			RefreshIn: 1800,
			Token:     "slow-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "ctx-cancel.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err = exchanger.Exchange(ctx, "test-token")
	if err == nil {
		t.Fatal("Exchange should fail with cancelled context")
	}
}

// --- Expiry calculation from expires_at timestamp ---

func TestExchangeCopilotToken_ExpiryCalculation(t *testing.T) {
	// expires_at is 30 minutes from now
	expectedExpiry := time.Now().Add(30 * time.Minute)
	expiresAt := expectedExpiry.Unix()

	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := copilotTokenResponse{
			ExpiresAt: expiresAt,
			RefreshIn: 1500,
			Token:     "expiry-test-token",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, err := NewTokenStore(filepath.Join(dir, "expiry.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	exchanger := NewCopilotExchanger(ts, WithCopilotAPIURL(server.URL))
	token, err := exchanger.Exchange(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	// The token's ExpiresAt should be close to expectedExpiry (within 5 seconds tolerance)
	diff := token.ExpiresAt.Sub(expectedExpiry)
	if diff < -5*time.Second || diff > 5*time.Second {
		t.Errorf("ExpiresAt diff from expected = %v, want within 5 seconds", diff)
	}
}

// --- Helper ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
