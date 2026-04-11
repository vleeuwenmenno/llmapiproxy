package web

import (
	"encoding/json"
	"io"
	"net/url"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/stats"
)

// createTestUI creates a UI instance with a real registry and collector for testing.
// It writes a minimal config.yaml to a temp directory.
func createTestUI(t *testing.T) (*UI, func()) {
	t.Helper()

	// Create temp dir for config and tokens
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	tokenDir := filepath.Join(tmpDir, "tokens")
	os.MkdirAll(tokenDir, 0700)

	// Write a minimal config with copilot and codex backends
	configContent := `
server:
  listen: ":0"
  api_keys:
    - test-key
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
  - name: codex
    type: codex
    base_url: https://chatgpt.com/backend-api/codex
`
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfgMgr, err := config.NewManager(configPath)
	if err != nil {
		t.Fatalf("creating config manager: %v", err)
	}

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfgMgr.Get())

	collector := stats.NewCollector(1000)

	ui := NewUI(cfgMgr, collector, registry, nil)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return ui, cleanup
}

func TestOAuthStatus_ReturnsHTMXFragment(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()

	// Should contain our HTMX auto-refresh container
	if !strings.Contains(body, "oauth-status-container") {
		t.Error("expected oauth-status-container in response")
	}
	if !strings.Contains(body, "hx-get") {
		t.Error("expected hx-get attribute for HTMX auto-refresh")
	}
	if !strings.Contains(body, "oauth-backend-card") {
		t.Error("expected oauth-backend-card in response")
	}
	// Should show both backends
	if !strings.Contains(body, "copilot") {
		t.Error("expected copilot backend in response")
	}
	if !strings.Contains(body, "codex") {
		t.Error("expected codex backend in response")
	}
}

func TestOAuthStatus_ShowsNotConnectedForMissingToken(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Clear any pre-existing tokens so we can test the "not connected" state.
	for _, name := range []string{"copilot", "codex"} {
		b := ui.registry.Get(name)
		if b == nil {
			continue
		}
		if dh, ok := b.(backend.OAuthDisconnectHandler); ok {
			dh.Disconnect()
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Both backends should show "Not connected" since tokens were cleared
	if !strings.Contains(body, "Not connected") {
		t.Error("expected 'Not connected' status for backends without tokens")
	}
	// Codex should show "Connect" button since it's not authenticated
	if !strings.Contains(body, "Connect") {
		t.Error("expected Connect button for unauthenticated Codex backend")
	}
}

func TestOAuthStatus_ShowsVisualIndicators(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Get the codex backend and store a valid token
	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	statusProvider := b.(backend.OAuthStatusProvider)
	store := b.(*backend.CodexBackend).GetTokenStore()

	// Store a valid token (expires in 1 hour)
	token := &oauth.TokenData{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now().Add(-5 * time.Minute),
		Source:      "codex_oauth",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	// Verify the status is now "valid"
	status := statusProvider.OAuthStatus()
	if status.TokenState != "valid" {
		t.Errorf("expected token state 'valid', got %q", status.TokenState)
	}
	if !status.Authenticated {
		t.Error("expected authenticated=true")
	}

	// Verify the template renders with the valid state
	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "oauth-status-dot-valid") {
		t.Error("expected green dot class for valid token")
	}
	if !strings.Contains(body, "Connected") {
		t.Error("expected 'Connected' text for valid token")
	}
}

func TestOAuthStatus_ShowsExpiringState(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	statusProvider := b.(backend.OAuthStatusProvider)
	store := b.(*backend.CodexBackend).GetTokenStore()

	// Store a token expiring in 2 minutes (within the 5-minute warning threshold)
	token := &oauth.TokenData{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(2 * time.Minute),
		ObtainedAt:  time.Now().Add(-28 * time.Minute),
		Source:      "codex_oauth",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	status := statusProvider.OAuthStatus()
	if status.TokenState != "expiring" {
		t.Errorf("expected token state 'expiring', got %q", status.TokenState)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "oauth-status-dot-expiring") {
		t.Error("expected yellow dot class for expiring token")
	}
}

func TestOAuthStatus_ShowsExpiredState(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	statusProvider := b.(backend.OAuthStatusProvider)
	store := b.(*backend.CodexBackend).GetTokenStore()

	// Store an expired token
	token := &oauth.TokenData{
		AccessToken:  "test-access-token",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		RefreshToken: "",
		Source:       "codex_oauth",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	status := statusProvider.OAuthStatus()
	if status.TokenState != "expired" {
		t.Errorf("expected token state 'expired', got %q", status.TokenState)
	}
	if !status.NeedsReauth {
		t.Error("expected needs_reauth=true for expired token with no refresh token")
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "oauth-status-dot-expired") {
		t.Error("expected red dot class for expired token")
	}
}

func TestOAuthStatus_DisplaysTokenMetadata(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	store := b.(*backend.CodexBackend).GetTokenStore()

	obtainedAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	expiresAt := time.Now().Add(50 * time.Minute).Truncate(time.Second)

	token := &oauth.TokenData{
		AccessToken: "test-access-token",
		ExpiresAt:   expiresAt,
		ObtainedAt:  obtainedAt,
		Source:      "codex_oauth",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Should show token source
	if !strings.Contains(body, "codex_oauth") {
		t.Error("expected token source 'codex_oauth' in response")
	}
	// Should show last refresh time (HTML-encoded; the + in timezone may be &#43;)
	refreshTime := obtainedAt.Format(time.RFC3339)
	if !strings.Contains(body, refreshTime) && !strings.Contains(body, strings.ReplaceAll(refreshTime, "+", "&#43;")) {
		t.Errorf("expected last refresh time %s in response", refreshTime)
	}
	// Should show expiry time (HTML-encoded; the + in timezone may be &#43;)
	expiryTime := expiresAt.Format(time.RFC3339)
	if !strings.Contains(body, expiryTime) && !strings.Contains(body, strings.ReplaceAll(expiryTime, "+", "&#43;")) {
		t.Errorf("expected expiry time %s in response", expiryTime)
	}
}

func TestOAuthStatus_CopilotShowsSource(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend not found")
	}
	store := b.(*backend.CopilotBackend).GetTokenStore()

	// Store a token with a specific source
	token := &oauth.TokenData{
		AccessToken: "test-github-token",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now().Add(-5 * time.Minute),
		Source:      "env:GH_TOKEN",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Should show the token source
	if !strings.Contains(body, "env:GH_TOKEN") {
		t.Error("expected token source 'env:GH_TOKEN' for copilot backend")
	}
}

func TestOAuthLogin_CodexRedirects(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)

	server := httptest.NewServer(r)
	defer server.Close()

	// Use a client that does not follow redirects so we can check the 302.
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get(server.URL + "/ui/oauth/login/codex")
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	// Should redirect (302) to the OAuth authorization URL
	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected status 302, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "openai.com") && !strings.Contains(location, "auth0.openai.com") {
		t.Errorf("expected redirect to OpenAI OAuth URL, got %s", location)
	}
	// Should have PKCE parameters
	if !strings.Contains(location, "code_challenge=") {
		t.Errorf("expected code_challenge in redirect URL, got %s", location)
	}
	if !strings.Contains(location, "state=") {
		t.Errorf("expected state in redirect URL, got %s", location)
	}
}

func TestOAuthLogin_UnknownBackendReturns404(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)

	server := httptest.NewServer(r)
	defer server.Close()

	resp, err := http.Get(server.URL + "/ui/oauth/login/nonexistent")
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", resp.StatusCode)
	}
}

func TestOAuthLogin_CopilotInitiatesDeviceCodeFlow(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)

	server := httptest.NewServer(r)
	defer server.Close()

	// Copilot now supports OAuth login via device code flow.
	// This will attempt to connect to the real GitHub device code endpoint,
	// which may succeed (200) or fail with a network error (500).
	// Either way, it should NOT return 400 (which was the old behavior
	// when Copilot didn\'t support OAuth login at all).
	resp, err := http.Get(server.URL + "/ui/oauth/login/copilot")
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	// Acceptable: 200 (device code page rendered) or 500 (network error reaching GitHub)
	if resp.StatusCode == http.StatusBadRequest {
		t.Errorf("got status 400, but Copilot should now support device code flow login")
	}
	// The response should either be the device code HTML page (200) or an error (500)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusInternalServerError {
		t.Logf("status %d (expected 200 or 500)", resp.StatusCode)
	}
}

func TestOAuthCallback_MissingParamsReturns400(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/callback/{backend}", ui.OAuthCallback)

	server := httptest.NewServer(r)
	defer server.Close()

	// Missing code and state
	resp, err := http.Get(server.URL + "/ui/oauth/callback/codex")
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
}

func TestOAuthCallback_ErrorParamRedirectsToSettings(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/callback/{backend}", ui.OAuthCallback)

	// Don't follow redirects
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Get("http://localhost" + "/ui/oauth/callback/codex?error=access_denied&error_description=User+denied+access")
	// Use the test server directly
	server := httptest.NewServer(r)
	defer server.Close()

	resp, err = client.Get(server.URL + "/ui/oauth/callback/codex?error=access_denied&error_description=User+denied+access")
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if !strings.Contains(location, "/ui/settings") {
		t.Errorf("expected redirect to /ui/settings, got %s", location)
	}
	if !strings.Contains(location, "OAuth+authentication+failed") {
		t.Errorf("expected error message in redirect URL, got %s", location)
	}
}

func TestOAuthDisconnect_ClearsToken(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Store a token for codex
	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	store := b.(*backend.CodexBackend).GetTokenStore()

	token := &oauth.TokenData{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:      "codex_oauth",
	}
	if err := store.Save(token); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	// Verify token exists
	if store.Get() == nil {
		t.Fatal("expected token to be stored")
	}

	r := chi.NewRouter()
	r.Post("/ui/oauth/disconnect/{backend}", ui.OAuthDisconnect)

	server := httptest.NewServer(r)
	defer server.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Post(server.URL+"/ui/oauth/disconnect/codex", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	// Should redirect to settings with success message
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected status 303, got %d", resp.StatusCode)
	}

	// Token should be cleared
	if store.Get() != nil {
		t.Error("expected token to be cleared after disconnect")
	}
}

func TestSettingsPage_ContainsOAuthSection(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/settings", nil)
	w := httptest.NewRecorder()

	ui.SettingsPage(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()

	// Should contain OAuth Connections section
	if !strings.Contains(body, "OAuth Connections") {
		t.Error("expected 'OAuth Connections' section in settings page")
	}
	// Should show both backends
	if !strings.Contains(body, "copilot") {
		t.Error("expected 'copilot' backend in settings page")
	}
	if !strings.Contains(body, "codex") {
		t.Error("expected 'codex' backend in settings page")
	}
}

func TestOAuthStatus_NoOAuthBackends(t *testing.T) {
	// Create a config with only openai backends
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	configContent := `
server:
  listen: ":0"
  api_keys:
    - test-key
backends:
  - name: openrouter
    type: openai
    base_url: https://openrouter.ai/api/v1
    api_key: test-or-key
`
	os.WriteFile(configPath, []byte(configContent), 0600)

	cfgMgr, err := config.NewManager(configPath)
	if err != nil {
		t.Fatalf("creating config manager: %v", err)
	}

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfgMgr.Get())
	collector := stats.NewCollector(1000)
	ui := NewUI(cfgMgr, collector, registry, nil)

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Should show the "no OAuth backends" message
	if !strings.Contains(body, "No OAuth backends configured") {
		t.Error("expected 'No OAuth backends configured' message when no OAuth backends exist")
	}
}

func TestOAuthStatus_MultipleBackendsDisplayed(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Store tokens for both backends
	copilotB := ui.registry.Get("copilot")
	codexB := ui.registry.Get("codex")
	if copilotB == nil || codexB == nil {
		t.Fatal("expected both copilot and codex backends")
	}

	copilotStore := copilotB.(*backend.CopilotBackend).GetTokenStore()
	codexStore := codexB.(*backend.CodexBackend).GetTokenStore()

	copilotStore.Save(&oauth.TokenData{
		AccessToken: "copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now().Add(-10 * time.Minute),
		Source:      "env:GH_TOKEN",
	})

	codexStore.Save(&oauth.TokenData{
		AccessToken: "codex-token",
		ExpiresAt:   time.Now().Add(45 * time.Minute),
		ObtainedAt:  time.Now().Add(-5 * time.Minute),
		Source:      "codex_oauth",
	})

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()
	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Both backends should be displayed
	if strings.Count(body, "oauth-backend-card") < 2 {
		t.Error("expected at least 2 oauth-backend-card elements")
	}
	if strings.Count(body, "oauth-status-dot-valid") < 2 {
		t.Error("expected at least 2 valid status dots")
	}
	if !strings.Contains(body, "env:GH_TOKEN") {
		t.Error("expected copilot source 'env:GH_TOKEN'")
	}
	if !strings.Contains(body, "codex_oauth") {
		t.Error("expected codex source 'codex_oauth'")
	}
}

func TestSettingsPage_IncludesHTMX(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/settings", nil)
	w := httptest.NewRecorder()

	ui.SettingsPage(w, req)

	body := w.Body.String()

	// Should include HTMX script
	if !strings.Contains(body, "htmx.min.js") {
		t.Error("expected HTMX script include in settings page")
	}
}

func TestSettingsPage_IncludesOAuthCSS(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/settings", nil)
	w := httptest.NewRecorder()

	ui.SettingsPage(w, req)

	body := w.Body.String()

	// Verify all oauth-* CSS classes are defined in the style block
	cssClasses := []string{
		".oauth-backend-card",
		".oauth-backend-header",
		".oauth-backend-info",
		".oauth-backend-name-row",
		".oauth-status-dot",
		".oauth-status-dot-valid",
		".oauth-status-dot-expiring",
		".oauth-status-dot-expired",
		".oauth-backend-name",
		".oauth-backend-type-badge",
		".oauth-backend-details",
		".oauth-detail-label",
		".oauth-detail-value",
		".oauth-text-valid",
		".oauth-text-expiring",
		".oauth-text-missing",
		".oauth-backend-actions",
		".btn-oauth-primary",
		".btn-oauth-danger",
		".oauth-backend-meta",
		".oauth-meta-item",
		".oauth-meta-label",
		".oauth-meta-value",
		".oauth-meta-warning",
		".oauth-expiry-valid",
		".oauth-expiry-expiring",
		".oauth-expiry-expired",
		".oauth-empty",
	}

	for _, cls := range cssClasses {
		if !strings.Contains(body, cls) {
			t.Errorf("expected CSS class %s definition in settings page style block", cls)
		}
	}
}

func TestOAuthStatus_CopilotShowsCheckStatusButton(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// Copilot backend should show a Check Status button
	if !strings.Contains(body, "Check Status") {
		t.Error("expected 'Check Status' button for Copilot backend")
	}
	// The Check Status button should use HTMX to refresh the status
	if !strings.Contains(body, "btn-oauth-secondary") {
		t.Error("expected btn-oauth-secondary class for Check Status button")
	}
}

func TestOAuthStatus_CheckStatusButtonUsesHTMX(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	// The Check Status button should have hx-get for HTMX-driven refresh
	if !strings.Contains(body, `hx-get="/ui/oauth/status"`) {
		t.Error("expected hx-get attribute on Check Status button for HTMX auto-refresh")
	}
	if !strings.Contains(body, `hx-target="#oauth-status-container"`) {
		t.Error("expected hx-target attribute pointing to oauth-status-container")
	}
}

func TestSettingsPage_OAuthCSSDarkLightMode(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/settings", nil)
	w := httptest.NewRecorder()

	ui.SettingsPage(w, req)

	body := w.Body.String()

	// Dark mode colors (CSS variables) should be defined in :root
	darkModeVars := []string{"--green", "--red", "--amber"}
	for _, v := range darkModeVars {
		if !strings.Contains(body, v) {
			t.Errorf("expected CSS variable %s for dark mode token colors", v)
		}
	}

	// Light mode overrides should exist
	if !strings.Contains(body, "body.light") {
		t.Error("expected body.light CSS rules for light mode token colors")
	}
}

// createTestUIWithTokenURL creates a test UI with a codex backend that uses
// the given tokenURL for the OAuth token exchange endpoint.
func createTestUIWithTokenURL(t *testing.T, tokenURL string) (*UI, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	tokenDir := filepath.Join(tmpDir, "tokens")
	os.MkdirAll(tokenDir, 0700)

	configContent := `
server:
  listen: ":0"
  api_keys:
    - test-key
backends:
  - name: copilot
    type: copilot
    base_url: https://api.githubcopilot.com
  - name: codex
    type: codex
    base_url: https://chatgpt.com/backend-api/codex
    oauth:
      token_url: "` + tokenURL + `"
`
	if err := os.WriteFile(configPath, []byte(configContent), 0600); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	cfgMgr, err := config.NewManager(configPath)
	if err != nil {
		t.Fatalf("creating config manager: %v", err)
	}

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfgMgr.Get())

	collector := stats.NewCollector(1000)

	ui := NewUI(cfgMgr, collector, registry, nil)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return ui, cleanup
}

func TestOAuthCallback_FullFlow(t *testing.T) {
	// Set up a mock token server that exchanges the code.
	var receivedCode string
	var receivedRedirectURI string
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		values, _ := url.ParseQuery(string(body))
		receivedCode = values.Get("code")
		receivedRedirectURI = values.Get("redirect_uri")

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email",
		})
	}))
	defer tokenServer.Close()

	ui, cleanup := createTestUIWithTokenURL(t, tokenServer.URL)
	defer cleanup()

	// Set up routes.
	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)
	r.Get("/ui/oauth/callback/{backend}", ui.OAuthCallback)

	loginServer := httptest.NewServer(r)
	defer loginServer.Close()

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Step 1: Initiate login to get the authorize URL and state.
	resp, err := client.Get(loginServer.URL + "/ui/oauth/login/codex")
	if err != nil {
		t.Fatalf("login request error: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302, got %d", resp.StatusCode)
	}

	location := resp.Header.Get("Location")
	locURL, err := url.Parse(location)
	if err != nil {
		t.Fatalf("parsing location: %v", err)
	}

	state := locURL.Query().Get("state")
	if state == "" {
		t.Fatal("state parameter missing from authorize URL")
	}

	redirectURI := locURL.Query().Get("redirect_uri")
	// Verify the redirect URI uses the backend name and correct path.
	if !strings.Contains(redirectURI, "/ui/oauth/callback/codex") {
		t.Errorf("redirect_uri = %q, should contain /ui/oauth/callback/codex", redirectURI)
	}

	// Step 2: Simulate the callback (as if the OAuth provider redirected back).
	callbackURL := loginServer.URL + "/ui/oauth/callback/codex?code=test-auth-code&state=" + state
	resp, err = client.Get(callbackURL)
	if err != nil {
		t.Fatalf("callback request error: %v", err)
	}
	resp.Body.Close()

	// Should redirect to settings on success.
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect to settings, got %d", resp.StatusCode)
	}

	finalLocation := resp.Header.Get("Location")
	if !strings.Contains(finalLocation, "/ui/settings") {
		t.Errorf("expected redirect to /ui/settings, got %s", finalLocation)
	}
	if !strings.Contains(finalLocation, "authentication+successful") {
		t.Errorf("expected success message in redirect, got %s", finalLocation)
	}

	// Verify the token was stored.
	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	codexBackend := b.(*backend.CodexBackend)
	store := codexBackend.GetTokenStore()
	token := store.Get()
	if token == nil {
		t.Fatal("token was not stored after callback")
	}
	if token.AccessToken != "test-access-token" {
		t.Errorf("access token = %q, want %q", token.AccessToken, "test-access-token")
	}

	// Verify the code exchange was called with the correct code and redirect URI.
	if receivedCode != "test-auth-code" {
		t.Errorf("code exchange received code %q, want %q", receivedCode, "test-auth-code")
	}
	if !strings.Contains(receivedRedirectURI, "/ui/oauth/callback/codex") {
		t.Errorf("code exchange redirect_uri = %q, should contain /ui/oauth/callback/codex", receivedRedirectURI)
	}
}
