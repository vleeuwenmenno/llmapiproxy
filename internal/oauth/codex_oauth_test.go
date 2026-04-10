package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ============================================================================
// PKCE Tests
// ============================================================================

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error: %v", err)
	}

	// Verifier should be non-empty.
	if pkce.Verifier == "" {
		t.Fatal("PKCE verifier is empty")
	}
	// Challenge should be non-empty.
	if pkce.Challenge == "" {
		t.Fatal("PKCE challenge is empty")
	}

	// Verify the challenge is the SHA-256 hash of the verifier, base64url-encoded.
	hash := sha256.Sum256([]byte(pkce.Verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(hash[:])
	if pkce.Challenge != expectedChallenge {
		t.Errorf("PKCE challenge mismatch: got %q, want %q", pkce.Challenge, expectedChallenge)
	}
}

func TestGeneratePKCE_Uniqueness(t *testing.T) {
	// Generate multiple PKCE pairs and verify they're unique.
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		pkce, err := GeneratePKCE()
		if err != nil {
			t.Fatalf("GeneratePKCE() iteration %d error: %v", i, err)
		}
		if seen[pkce.Verifier] {
			t.Fatalf("duplicate verifier generated at iteration %d", i)
		}
		seen[pkce.Verifier] = true
	}
}

func TestGeneratePKCE_VerifierLength(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error: %v", err)
	}

	// The verifier is base64url-encoded from 32 random bytes.
	// Base64 encoding of 32 bytes = 43 chars (no padding).
	decoded, err := base64.RawURLEncoding.DecodeString(pkce.Verifier)
	if err != nil {
		t.Fatalf("verifier is not valid base64url: %v", err)
	}
	if len(decoded) != pkceVerifierLength {
		t.Errorf("verifier decodes to %d bytes, want %d", len(decoded), pkceVerifierLength)
	}
}

func TestGeneratePKCE_S256Method(t *testing.T) {
	// Verify S256 method: challenge = BASE64URL(SHA256(verifier))
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() error: %v", err)
	}

	// Manually compute what the challenge should be.
	hash := sha256.Sum256([]byte(pkce.Verifier))
	challenge := base64.RawURLEncoding.EncodeToString(hash[:])

	if pkce.Challenge != challenge {
		t.Errorf("S256 challenge mismatch:\n  got:  %q\n  want: %q", pkce.Challenge, challenge)
	}

	// Verify no padding characters.
	if strings.Contains(pkce.Challenge, "=") {
		t.Error("PKCE challenge contains padding characters")
	}
	if strings.Contains(pkce.Verifier, "=") {
		t.Error("PKCE verifier contains padding characters")
	}
}

// ============================================================================
// Authorize URL Tests
// ============================================================================

func TestAuthorizeURL(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    "https://auth.example.com/token",
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid profile email",
	})

	authURL, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	if state == "" {
		t.Fatal("state is empty")
	}

	// Parse the URL and verify parameters.
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}

	// Check required OAuth parameters.
	tests := []struct {
		param string
		want  string
	}{
		{"response_type", "code"},
		{"client_id", "test-client-id"},
		{"redirect_uri", "https://callback.example.com/oauth/callback"},
		{"scope", "openid profile email"},
		{"code_challenge_method", "S256"},
		{"state", state},
	}
	for _, tt := range tests {
		got := u.Query().Get(tt.param)
		if got != tt.want {
			t.Errorf("authorize URL param %q = %q, want %q", tt.param, got, tt.want)
		}
	}

	// code_challenge should be non-empty.
	if u.Query().Get("code_challenge") == "" {
		t.Error("authorize URL missing code_challenge")
	}
}

func TestAuthorizeURL_StoresPendingState(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	_, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	// Verify the pending state was stored.
	if handler.PendingStateCount() != 1 {
		t.Errorf("pending state count = %d, want 1", handler.PendingStateCount())
	}

	pending := handler.GetPendingState(state)
	if pending == nil {
		t.Fatal("pending state not found")
	}
	if pending.State != state {
		t.Errorf("pending state = %q, want %q", pending.State, state)
	}
}

func TestAuthorizeURL_PKCENotExposed(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	_, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	// GetPendingState should return nil for PKCE (security measure).
	pending := handler.GetPendingState(state)
	if pending == nil {
		t.Fatal("pending state not found")
	}
	if pending.PKCE != nil {
		t.Error("PKCE verifier should not be exposed via GetPendingState")
	}
}

func TestAuthorizeURL_MultipleFlows(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	_, state1, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() #1 error: %v", err)
	}
	_, state2, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() #2 error: %v", err)
	}

	if state1 == state2 {
		t.Error("two authorization flows should have different state values")
	}
	if handler.PendingStateCount() != 2 {
		t.Errorf("pending state count = %d, want 2", handler.PendingStateCount())
	}
}

// ============================================================================
// Callback Handler Tests
// ============================================================================

func TestHandleCallback_Success(t *testing.T) {
	// Set up a mock token server.
	var receivedCode string
	var receivedVerifier string
	var receivedRedirectURI string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		receivedCode = values.Get("code")
		receivedVerifier = values.Get("code_verifier")
		receivedRedirectURI = values.Get("redirect_uri")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "test-access-token",
			RefreshToken: "test-refresh-token",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
			Scope:        "openid profile email",
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid profile email",
	})

	// Initiate the flow to create pending state.
	_, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	// Handle the callback.
	tokenData, err := handler.HandleCallback(context.Background(), "test-auth-code", state)
	if err != nil {
		t.Fatalf("HandleCallback() error: %v", err)
	}

	// Verify token data.
	if tokenData.AccessToken != "test-access-token" {
		t.Errorf("access token = %q, want %q", tokenData.AccessToken, "test-access-token")
	}
	if tokenData.RefreshToken != "test-refresh-token" {
		t.Errorf("refresh token = %q, want %q", tokenData.RefreshToken, "test-refresh-token")
	}
	if tokenData.Source != "codex_oauth" {
		t.Errorf("source = %q, want %q", tokenData.Source, "codex_oauth")
	}

	// Verify the token was persisted.
	loaded := store.Get()
	if loaded == nil || loaded.AccessToken != "test-access-token" {
		t.Error("token was not persisted to store")
	}

	// Verify the code exchange request.
	if receivedCode != "test-auth-code" {
		t.Errorf("code exchange sent code %q, want %q", receivedCode, "test-auth-code")
	}
	if receivedVerifier == "" {
		t.Error("code exchange did not send code_verifier")
	}
	if receivedRedirectURI != "https://callback.example.com/oauth/callback" {
		t.Errorf("redirect_uri = %q, want %q", receivedRedirectURI, "https://callback.example.com/oauth/callback")
	}

	// Verify the pending state was removed (one-time use).
	if handler.PendingStateCount() != 0 {
		t.Errorf("pending state count after callback = %d, want 0", handler.PendingStateCount())
	}
}

func TestHandleCallback_InvalidState(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	// Try to handle callback with a state that was never initiated.
	_, err := handler.HandleCallback(context.Background(), "some-code", "invalid-state")
	if err == nil {
		t.Fatal("expected error for invalid state, got nil")
	}
	if !strings.Contains(err.Error(), "invalid or expired state") {
		t.Errorf("error = %q, want error containing 'invalid or expired state'", err.Error())
	}
}

func TestHandleCallback_MissingState(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	// Empty state.
	_, err := handler.HandleCallback(context.Background(), "some-code", "")
	if err == nil {
		t.Fatal("expected error for missing state, got nil")
	}
}

func TestHandleCallback_ReplayProtection(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "test-access-token",
			RefreshToken: "test-refresh-token",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid",
	})

	_, state, _ := handler.AuthorizeURL()

	// First callback should succeed.
	_, err := handler.HandleCallback(context.Background(), "test-code", state)
	if err != nil {
		t.Fatalf("first callback error: %v", err)
	}

	// Second callback with the same state should fail (replay protection).
	_, err = handler.HandleCallback(context.Background(), "test-code", state)
	if err == nil {
		t.Fatal("expected error for replayed state, got nil")
	}
	if !strings.Contains(err.Error(), "invalid or expired state") {
		t.Errorf("replay error = %q, want 'invalid or expired state'", err.Error())
	}
}

func TestHandleCallback_TokenExchangeFailure(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid",
	})

	_, state, _ := handler.AuthorizeURL()

	_, err := handler.HandleCallback(context.Background(), "bad-code", state)
	if err == nil {
		t.Fatal("expected error for failed token exchange, got nil")
	}
	if !strings.Contains(err.Error(), "token exchange failed") {
		t.Errorf("error = %q, want 'token exchange failed'", err.Error())
	}
}

// ============================================================================
// Token Refresh Tests
// ============================================================================

func TestRefreshToken_Success(t *testing.T) {
	var receivedRefreshToken string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		receivedRefreshToken = values.Get("refresh_token")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "new-access-token",
			RefreshToken: "rotated-refresh-token",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
			Scope:        "openid profile email",
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	// Store an initial token with a refresh token.
	store.Save(&TokenData{
		AccessToken:  "old-access-token",
		RefreshToken: "old-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // expired
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid",
	})

	tokenData, err := handler.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("RefreshToken() error: %v", err)
	}

	// Verify the new token.
	if tokenData.AccessToken != "new-access-token" {
		t.Errorf("access token = %q, want %q", tokenData.AccessToken, "new-access-token")
	}

	// Verify rotation: new refresh token.
	if tokenData.RefreshToken != "rotated-refresh-token" {
		t.Errorf("refresh token = %q, want %q (rotated)", tokenData.RefreshToken, "rotated-refresh-token")
	}

	// Verify the old refresh token was sent in the request.
	if receivedRefreshToken != "old-refresh-token" {
		t.Errorf("refresh request used token %q, want %q", receivedRefreshToken, "old-refresh-token")
	}

	// Verify the store was updated with the new token.
	stored := store.Get()
	if stored.RefreshToken != "rotated-refresh-token" {
		t.Errorf("stored refresh token = %q, want %q (rotation should be persisted)", stored.RefreshToken, "rotated-refresh-token")
	}
}

func TestRefreshToken_NoToken(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	_, err := handler.RefreshToken(context.Background())
	if err == nil {
		t.Fatal("expected error when no token, got nil")
	}
	if !strings.Contains(err.Error(), "no token available") {
		t.Errorf("error = %q, want 'no token available'", err.Error())
	}
}

func TestRefreshToken_NoRefreshToken(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	store.Save(&TokenData{
		AccessToken: "access-only",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:       "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, nil)

	_, err := handler.RefreshToken(context.Background())
	if err == nil {
		t.Fatal("expected error when no refresh token, got nil")
	}
	if !strings.Contains(err.Error(), "no refresh token") {
		t.Errorf("error = %q, want 'no refresh token'", err.Error())
	}
}

func TestRefreshToken_RefreshTokenRevoked(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid_grant","error_description":"The refresh token is invalid or expired."}`))
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	store.Save(&TokenData{
		AccessToken:  "expired-access",
		RefreshToken: "revoked-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID: "test-client-id",
		AuthURL:  "https://auth.example.com/authorize",
		TokenURL: tokenServer.URL,
		Scope:    "openid",
	})

	_, err := handler.RefreshToken(context.Background())
	if err == nil {
		t.Fatal("expected error for revoked refresh token, got nil")
	}
	if !strings.Contains(err.Error(), "refresh failed") {
		t.Errorf("error = %q, want 'refresh failed'", err.Error())
	}
}

func TestRefreshToken_Rotation(t *testing.T) {
	callCount := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  fmt.Sprintf("access-token-%d", callCount),
			RefreshToken: fmt.Sprintf("refresh-token-%d", callCount),
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	// Start with an initial token.
	store.Save(&TokenData{
		AccessToken:  "initial-access",
		RefreshToken: "initial-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID: "test-client-id",
		AuthURL:  "https://auth.example.com/authorize",
		TokenURL: tokenServer.URL,
		Scope:    "openid",
	})

	// First refresh: should use initial-refresh and get refresh-token-1.
	token1, err := handler.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("first refresh error: %v", err)
	}
	if token1.RefreshToken != "refresh-token-1" {
		t.Errorf("first refresh token = %q, want %q", token1.RefreshToken, "refresh-token-1")
	}

	// Second refresh: should use refresh-token-1 and get refresh-token-2.
	token2, err := handler.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("second refresh error: %v", err)
	}
	if token2.RefreshToken != "refresh-token-2" {
		t.Errorf("second refresh token = %q, want %q", token2.RefreshToken, "refresh-token-2")
	}

	// Verify the store has the latest token.
	stored := store.Get()
	if stored.RefreshToken != "refresh-token-2" {
		t.Errorf("stored refresh token = %q, want %q (latest rotation)", stored.RefreshToken, "refresh-token-2")
	}
}

// ============================================================================
// PKCE Verifier Security Tests
// ============================================================================

func TestPKCEVerifier_NeverPersisted(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	// Generate an authorization URL (creates a PKCE verifier).
	_, _, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("AuthorizeURL() error: %v", err)
	}

	// Verify the PKCE verifier is not in the token store.
	tokenData := store.Get()
	if tokenData != nil {
		t.Error("token store should be empty before callback")
	}

	// Read the token file directly — it should not exist or not contain verifier.
	data, err := os.ReadFile(store.filePath)
	if err == nil {
		if strings.Contains(string(data), "verifier") || strings.Contains(string(data), "code_verifier") {
			t.Error("token file contains PKCE verifier reference")
		}
	}
}

func TestPKCEVerifier_NotInTokenData(t *testing.T) {
	// Complete a full OAuth flow and verify the persisted token doesn't
	// contain the PKCE verifier.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "test-access",
			RefreshToken: "test-refresh",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid",
	})

	_, state, _ := handler.AuthorizeURL()
	_, err := handler.HandleCallback(context.Background(), "test-code", state)
	if err != nil {
		t.Fatalf("HandleCallback() error: %v", err)
	}

	// Read the persisted token file.
	data, err := os.ReadFile(store.filePath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}

	// Verify no PKCE-related fields in the JSON.
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parsing token JSON: %v", err)
	}
	for key := range m {
		if strings.Contains(strings.ToLower(key), "verifier") || strings.Contains(strings.ToLower(key), "pkce") {
			t.Errorf("token file contains PKCE-related field: %q", key)
		}
	}
}

// ============================================================================
// State Cleanup Tests
// ============================================================================

func TestStateCleanup_OnCallback(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	// Create a pending state.
	 handler.AuthorizeURL()
	if handler.PendingStateCount() != 1 {
		t.Fatalf("pending state count = %d, want 1", handler.PendingStateCount())
	}

	// The pending state should be removed after callback (even if it fails).
	// We'll use an invalid code but a valid state.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer tokenServer.Close()

	handler.SetHTTPClient(&http.Client{Timeout: 10 * time.Second})
	// We need to re-create with the correct token URL
	handler2 := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID:    "test-client-id",
		AuthURL:     "https://auth.example.com/authorize",
		TokenURL:    tokenServer.URL,
		RedirectURI: "https://callback.example.com/oauth/callback",
		Scope:       "openid",
	})

	// Create state on handler2.
	_, state2, _ := handler2.AuthorizeURL()

	handler2.HandleCallback(context.Background(), "code", state2)
	if handler2.PendingStateCount() != 0 {
		t.Errorf("pending state after failed callback = %d, want 0", handler2.PendingStateCount())
	}
}

func TestStateCleanup_OnTimeout(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	// Create handler with a very short timeout for testing.
	handler := NewCodexOAuthHandler(store, nil)

	// Manually add a pending state with an old timestamp to simulate expiry.
	handler.mu.Lock()
	expiredState := &PendingOAuthState{
		State:       "expired-state",
		PKCE:        &PKCEPair{Verifier: "test-verifier", Challenge: "test-challenge"},
		CreatedAt:   time.Now().Add(-codexOAuthTimeout - 1*time.Minute), // expired
		RedirectURI: "http://localhost:8000/callback",
	}
	handler.pending["expired-state"] = expiredState

	// Also add a non-expired state.
	freshState := &PendingOAuthState{
		State:       "fresh-state",
		PKCE:        &PKCEPair{Verifier: "fresh-verifier", Challenge: "fresh-challenge"},
		CreatedAt:   time.Now(), // fresh
		RedirectURI: "http://localhost:8000/callback",
	}
	handler.pending["fresh-state"] = freshState
	handler.mu.Unlock()

	// Run cleanup manually (simulate what the background goroutine does).
	handler.mu.Lock()
	for state, pending := range handler.pending {
		if pending.IsExpired() {
			delete(handler.pending, state)
		}
	}
	handler.mu.Unlock()

	// The expired state should be cleaned up.
	if handler.GetPendingState("expired-state") != nil {
		t.Error("expired state should have been cleaned up")
	}

	// The fresh state should still exist.
	if handler.GetPendingState("fresh-state") == nil {
		t.Error("fresh state should not have been cleaned up")
	}
}

func TestStateCleanup_LateCallback(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	handler := NewCodexOAuthHandler(store, nil)

	// Manually add an expired state.
	handler.mu.Lock()
	handler.pending["late-state"] = &PendingOAuthState{
		State:       "late-state",
		PKCE:        &PKCEPair{Verifier: "verifier", Challenge: "challenge"},
		CreatedAt:   time.Now().Add(-codexOAuthTimeout - 1*time.Second),
		RedirectURI: "http://localhost:8000/callback",
	}
	handler.mu.Unlock()

	// A late callback for the expired state should fail.
	_, err := handler.HandleCallback(context.Background(), "code", "late-state")
	if err == nil {
		t.Fatal("expected error for expired state, got nil")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %q, want error containing 'expired'", err.Error())
	}
}

// ============================================================================
// Upstream 401 Re-auth Retry Tests
// ============================================================================

func TestRefreshWithRetry_CachedTokenValid(t *testing.T) {
	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	// Store a valid (non-expired) token.
	store.Save(&TokenData{
		AccessToken:  "valid-access",
		RefreshToken: "valid-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, nil)

	tokenData, err := handler.RefreshWithRetry(context.Background())
	if err != nil {
		t.Fatalf("RefreshWithRetry() error: %v", err)
	}
	if tokenData.AccessToken != "valid-access" {
		t.Errorf("access token = %q, want %q", tokenData.AccessToken, "valid-access")
	}
}

func TestRefreshWithRetry_ExpiredTokenRefreshes(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "refreshed-access",
			RefreshToken: "rotated-refresh",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	store.Save(&TokenData{
		AccessToken:  "expired-access",
		RefreshToken: "expired-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour), // expired
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID: "test-client-id",
		AuthURL:  "https://auth.example.com/authorize",
		TokenURL: tokenServer.URL,
		Scope:    "openid",
	})

	tokenData, err := handler.RefreshWithRetry(context.Background())
	if err != nil {
		t.Fatalf("RefreshWithRetry() error: %v", err)
	}
	if tokenData.AccessToken != "refreshed-access" {
		t.Errorf("access token = %q, want %q", tokenData.AccessToken, "refreshed-access")
	}
}

func TestRefreshWithRetry_ConcurrentRefresh(t *testing.T) {
	// Simulate concurrent refresh requests — only one should hit the token server.
	callCount := 0
	var mu sync.Mutex
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()

		// Add a small delay to simulate network latency.
		time.Sleep(100 * time.Millisecond)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tokenResponse{
			AccessToken:  "concurrent-access",
			RefreshToken: "concurrent-refresh",
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		})
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	store.Save(&TokenData{
		AccessToken:  "expired-access",
		RefreshToken: "expired-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID: "test-client-id",
		AuthURL:  "https://auth.example.com/authorize",
		TokenURL: tokenServer.URL,
		Scope:    "openid",
	})

	// Launch multiple concurrent refresh attempts.
	var wg sync.WaitGroup
	errors := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := handler.RefreshWithRetry(context.Background())
			errors <- err
		}()
	}
	wg.Wait()
	close(errors)

	// All should succeed.
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent refresh error: %v", err)
		}
	}

	// Only one actual refresh call should have been made.
	mu.Lock()
	count := callCount
	mu.Unlock()
	if count != 1 {
		t.Errorf("token server called %d times, want 1 (single refresh coordination)", count)
	}
}

func TestRefreshWithRetry_StaleTokenFallback(t *testing.T) {
	// When refresh fails but the cached token is still usable (not expired),
	// the stale token should be returned.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer tokenServer.Close()

	store := newTestTokenStore(t)
	defer os.Remove(store.filePath)

	// Token needs refresh but hasn't expired yet.
	store.Save(&TokenData{
		AccessToken:  "stale-but-valid",
		RefreshToken: "stale-refresh",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(5 * time.Minute), // still valid for 5 min
		RefreshIn:    -1,                               // negative to force NeedsRefresh
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:        "codex_oauth",
	})

	handler := NewCodexOAuthHandler(store, &CodexOAuthConfig{
		ClientID: "test-client-id",
		AuthURL:  "https://auth.example.com/authorize",
		TokenURL: tokenServer.URL,
		Scope:    "openid",
	})

	// Force NeedsRefresh by using a token that needs refresh.
	// We need to bypass the "still valid" check by making the token expired
	// but with the refresh coordination fallback.

	// Actually let's test the real scenario: token is near-expiry, refresh fails.
	// The store says still valid because the token hasn't expired yet.
	tokenData, err := handler.RefreshWithRetry(context.Background())
	if err != nil {
		t.Fatalf("RefreshWithRetry() error: %v", err)
	}
	if tokenData.AccessToken != "stale-but-valid" {
		t.Errorf("expected stale token fallback, got %q", tokenData.AccessToken)
	}
}

// ============================================================================
// Default Config Tests
// ============================================================================

func TestDefaultCodexOAuthConfig(t *testing.T) {
	cfg := DefaultCodexOAuthConfig()
	if cfg.ClientID != defaultCodexClientID {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, defaultCodexClientID)
	}
	if cfg.AuthURL != defaultCodexAuthURL {
		t.Errorf("AuthURL = %q, want %q", cfg.AuthURL, defaultCodexAuthURL)
	}
	if cfg.TokenURL != defaultCodexTokenURL {
		t.Errorf("TokenURL = %q, want %q", cfg.TokenURL, defaultCodexTokenURL)
	}
	if cfg.RedirectURI != defaultCodexRedirectURI {
		t.Errorf("RedirectURI = %q, want %q", cfg.RedirectURI, defaultCodexRedirectURI)
	}
}

// ============================================================================
// Helper functions
// ============================================================================

func newTestTokenStore(t *testing.T) *TokenStore {
	t.Helper()
	dir := t.TempDir()
	ts, err := NewTokenStore(filepath.Join(dir, "test-token.json"))
	if err != nil {
		t.Fatalf("creating test token store: %v", err)
	}
	return ts
}
