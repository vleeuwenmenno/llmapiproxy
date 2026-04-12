package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
)

func TestCodexLoopbackHandler_Success(t *testing.T) {
	var receivedCode string
	var receivedRedirectURI string
	tokenServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		receivedCode = values.Get("code")
		receivedRedirectURI = values.Get("redirect_uri")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email offline_access",
		})
	}))
	defer tokenServer.Close()

	registry := newLoopbackTestRegistry(t, tokenServer.URL)
	codexBackend := registry.Get("codex").(*backend.CodexBackend)

	authURL, state, err := codexBackend.InitiateLogin()
	if err != nil {
		t.Fatalf("InitiateLogin() error: %v", err)
	}
	if !strings.Contains(authURL, url.QueryEscape("http://localhost:1455/auth/callback")) {
		t.Fatalf("authorize URL missing loopback redirect_uri: %s", authURL)
	}
	if state == "" {
		t.Fatal("state should not be empty")
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=test-auth-code&state="+url.QueryEscape(state), nil)
	w := httptest.NewRecorder()

	newCodexLoopbackHandler(registry, "http://localhost:8000").ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "/ui/models") {
		t.Fatalf("expected redirect to models, got %s", location)
	}
	if !strings.Contains(location, "codex+authentication+successful") {
		t.Fatalf("expected success message in redirect, got %s", location)
	}
	if receivedCode != "test-auth-code" {
		t.Fatalf("received code = %q, want %q", receivedCode, "test-auth-code")
	}
	if receivedRedirectURI != "http://localhost:1455/auth/callback" {
		t.Fatalf("received redirect_uri = %q, want %q", receivedRedirectURI, "http://localhost:1455/auth/callback")
	}
	if codexBackend.GetTokenStore().Get() == nil {
		t.Fatal("expected token to be stored after successful callback")
	}
}

func TestCodexLoopbackHandler_UnknownStateRedirectsToSettings(t *testing.T) {
	registry := newLoopbackTestRegistry(t, "https://auth.example.com/token")

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=test-auth-code&state=missing", nil)
	w := httptest.NewRecorder()

	newCodexLoopbackHandler(registry, "http://localhost:8000").ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 redirect, got %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	if !strings.Contains(location, "no+pending+codex+oauth+flow+matched+the+callback+state") {
		t.Fatalf("expected loopback error in redirect, got %s", location)
	}
}

func TestRegistry_HandleCodexLoopbackCallback_NoPendingState(t *testing.T) {
	registry := newLoopbackTestRegistry(t, "https://auth.example.com/token")

	backendName, err := registry.HandleCodexLoopbackCallback(context.Background(), "code", "missing")
	if err == nil {
		t.Fatal("expected error for missing pending state")
	}
	if backendName != "" {
		t.Fatalf("backend name = %q, want empty", backendName)
	}
}

func newLoopbackTestRegistry(t *testing.T, tokenURL string) *backend.Registry {
	t.Helper()

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
					TokenURL:  tokenURL,
					TokenPath: filepath.Join(t.TempDir(), "codex-token.json"),
				},
			},
		},
	}

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfg)
	return registry
}
