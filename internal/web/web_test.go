package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	ui := NewUI(cfgMgr, collector, registry, nil, nil, nil, nil)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return ui, cleanup
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
	if strings.Contains(body, "Status:") {
		t.Error("expected standalone OAuth cards not to keep the textual status label in the header")
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
	if !strings.Contains(body, `data-authenticated="true"`) {
		t.Error("expected oauth status card to expose authenticated state for polling")
	}
	if !strings.Contains(body, `data-token-state="valid"`) {
		t.Error("expected oauth status card to expose token state for polling")
	}
	if !strings.Contains(body, "Connected") {
		t.Error("expected 'Connected' text for valid token")
	}
}

func TestDeviceCodeTemplate_PollsUsingOAuthStatusCardData(t *testing.T) {
	data, err := templateFS.ReadFile("templates/device_code.html")
	if err != nil {
		t.Fatalf("reading device code template: %v", err)
	}

	body := string(data)
	if !strings.Contains(body, "getElementById(`oauth-status-${backend}`)") {
		t.Error("expected device code template to look up the current oauth status card by id")
	}
	if !strings.Contains(body, "card.dataset.authenticated") {
		t.Error("expected device code template to read authenticated state from data attributes")
	}
	if !strings.Contains(body, "card.dataset.tokenState") {
		t.Error("expected device code template to read token state from data attributes")
	}
	if !strings.Contains(body, "Authorization timed out") {
		t.Error("expected device code template to stop polling after device code expiry")
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

	if !strings.Contains(body, "oauth-status-container") {
		t.Error("expected oauth-status-container in response")
	}
	if !strings.Contains(body, "hx-get") {
		t.Error("expected hx-get attribute for HTMX auto-refresh")
	}
	if !strings.Contains(body, "oauth-backend-card") {
		t.Error("expected oauth-backend-card in response")
	}
	if !strings.Contains(body, "copilot") {
		t.Error("expected copilot backend in response")
	}
	if !strings.Contains(body, "codex") {
		t.Error("expected codex backend in response")
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

	if !strings.Contains(body, `data-oauth-source="env:GH_TOKEN"`) {
		t.Error("expected token source hint metadata for copilot backend")
	}
	if strings.Contains(body, ">Source:") {
		t.Error("expected source label to stay out of the compact footer")
	}
}

func TestOAuthLogin_CodexRedirects(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)

	server := newTestServer(t, r)
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
	if !strings.Contains(location, url.QueryEscape(oauth.BuiltinCodexRedirectURI())) {
		t.Errorf("expected built-in Codex redirect URI %q, got %s", oauth.BuiltinCodexRedirectURI(), location)
	}
}

func TestOAuthLogin_UnknownBackendReturns404(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)

	server := newTestServer(t, r)
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

	server := newTestServer(t, r)
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

	server := newTestServer(t, r)
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
	server := newTestServer(t, r)
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
	if !strings.Contains(location, "/ui/models") {
		t.Errorf("expected redirect to /ui/models, got %s", location)
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

	server := newTestServer(t, r)
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

func TestModelsPage_ContainsOAuthSection(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()

	ui.ModelsPage(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body := w.Body.String()

	// Should contain oauth card section CSS (OAuth management moved to models page)
	if !strings.Contains(body, "oauth-card-section") {
		t.Error("expected oauth-card-section in models page")
	}
	// Should show both backends
	if !strings.Contains(body, "copilot") {
		t.Error("expected 'copilot' backend in models page")
	}
	if !strings.Contains(body, "codex") {
		t.Error("expected 'codex' backend in models page")
	}
	if !strings.Contains(body, "model-scroll") {
		t.Error("expected scrollable model list container in models page")
	}
	if strings.Contains(body, "show-more-btn") {
		t.Error("did not expect legacy show-more button styling in models page")
	}
}

func TestModelsPage_UsesCodexAndCopilotIcons(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()

	ui.ModelsPage(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `/ui/static/icons/codex-color.svg`) {
		t.Error("expected Codex icon asset on models page")
	}
	if !strings.Contains(body, `/ui/static/icons/githubcopilot.svg`) {
		t.Error("expected Copilot icon asset on models page")
	}
	if !strings.Contains(body, `backend-icon backend-icon-color`) {
		t.Error("expected colored icon class for Codex backend")
	}
}

func TestModelsPage_RendersOverlapToolbarShell(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()

	ui.ModelsPage(w, req)

	body := w.Body.String()
	checks := []string{
		`id="overlap-section"`,
		`id="overlap-search"`,
		`id="overlap-backend-filter"`,
		`id="overlap-status-filter"`,
		`id="overlap-strategy-filter"`,
		`id="overlap-sort"`,
		`id="overlap-view-toggle"`,
		`data-view="grid"`,
		`data-view="list"`,
		`var overlapBootstrap =`,
	}

	for _, check := range checks {
		if !strings.Contains(body, check) {
			t.Errorf("expected models page to contain %q", check)
		}
	}
}

func TestModelsPage_RendersHumanReadableOAuthFooter(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	store := b.(*backend.CodexBackend).GetTokenStore()
	expiresAt := time.Now().Add(26 * time.Hour).Truncate(time.Second)
	obtainedAt := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	if err := store.Save(&oauth.TokenData{
		AccessToken: "test-access-token",
		ExpiresAt:   expiresAt,
		ObtainedAt:  obtainedAt,
		Source:      "codex_oauth",
	}); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()
	ui.ModelsPage(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "oauth-footer") {
		t.Error("expected oauth footer layout on models page")
	}
	if !strings.Contains(body, "Connected") {
		t.Error("expected OAuth status text to be rendered in the footer")
	}
	if !strings.Contains(body, "Expires: tomorrow at") {
		t.Error("expected human-readable expiry text on models page")
	}
	if !strings.Contains(body, "Refreshed: 1 hour ago") {
		t.Error("expected human-readable refresh text on models page")
	}
	if !strings.Contains(body, `data-oauth-source="codex_oauth"`) {
		t.Error("expected source hint metadata on models page")
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
	ui := NewUI(cfgMgr, collector, registry, nil, nil, nil, nil)

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
	if !strings.Contains(body, `data-oauth-source="env:GH_TOKEN"`) {
		t.Error("expected Copilot source hint metadata")
	}
	if !strings.Contains(body, `data-oauth-source="codex_oauth"`) {
		t.Error("expected Codex source hint metadata")
	}
}

func TestModelsPage_IncludesHTMX(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()

	ui.ModelsPage(w, req)

	body := w.Body.String()

	// Should include HTMX script (needed for dynamic model loading)
	if !strings.Contains(body, "htmx.min.js") {
		t.Error("expected HTMX script include in models page")
	}
}

func TestModelsPage_IncludesOAuthCSS(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/ui/models", nil)
	w := httptest.NewRecorder()

	ui.ModelsPage(w, req)

	body := w.Body.String()

	// Verify oauth-* CSS classes used by backend cards are defined in models page
	cssClasses := []string{
		".oauth-status-dot",
		".oauth-status-dot-valid",
		".oauth-status-dot-expiring",
		".oauth-status-dot-expired",
		".oauth-text-valid",
		".oauth-text-expiring",
		".oauth-text-missing",
		".oauth-card-section",
		".oauth-footer",
		".oauth-actions",
		".oauth-meta-pill",
		".oauth-time-text",
		".oauth-source-hint",
		".btn-oauth-xs",
		".btn-oauth-xs-primary",
		".btn-oauth-xs-danger",
		".backend-icon-color",
	}

	for _, cls := range cssClasses {
		if !strings.Contains(body, cls) {
			t.Errorf("expected CSS class %s definition in models page style block", cls)
		}
	}
}

func TestOAuthStatus_CopilotShowsConnectButtonWhenMissingToken(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend not found")
	}
	if disconnecter, ok := b.(backend.OAuthDisconnectHandler); ok {
		if err := disconnecter.Disconnect(); err != nil {
			t.Fatalf("disconnecting copilot token: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	if !strings.Contains(body, `href="/ui/oauth/login/copilot"`) {
		t.Error("expected Copilot connect button to point to /ui/oauth/login/copilot")
	}
	if !strings.Contains(body, `target="_blank"`) {
		t.Error("expected OAuth connect links to open in a new tab")
	}
	if !strings.Contains(body, `rel="noopener noreferrer"`) {
		t.Error("expected OAuth connect links to use noopener noreferrer")
	}
	if !strings.Contains(body, "Connect") {
		t.Error("expected Copilot connect button label when no token is stored")
	}
}

func TestOAuthStatus_CodexConnectLinksOpenInNewTab(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	if disconnecter, ok := b.(backend.OAuthDisconnectHandler); ok {
		if err := disconnecter.Disconnect(); err != nil {
			t.Fatalf("disconnecting codex token: %v", err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	if !strings.Contains(body, `href="/ui/oauth/login/codex" class="btn btn-oauth-primary" target="_blank" rel="noopener noreferrer"`) {
		t.Error("expected Codex browser connect link to open in a new tab")
	}
	if !strings.Contains(body, `href="/ui/oauth/device-login/codex" class="btn btn-oauth-secondary" target="_blank" rel="noopener noreferrer"`) {
		t.Error("expected Codex device code link to open in a new tab")
	}
}

func TestOAuthStatus_CopilotCheckStatusUsesHTMXWhenTokenExists(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend not found")
	}
	copilotBackend := b.(*backend.CopilotBackend)
	if err := copilotBackend.GetTokenStore().Save(&oauth.TokenData{
		AccessToken: "copilot-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:      "device_code_flow",
	}); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()

	if !strings.Contains(body, `hx-post="/ui/oauth/check-status/copilot"`) {
		t.Error("expected hx-post attribute on Check Status button pointing to check-status endpoint")
	}
	if !strings.Contains(body, `hx-target="#oauth-status-copilot"`) {
		t.Error("expected hx-target attribute pointing to the copilot status card")
	}
	if !strings.Contains(body, `hx-disabled-elt="this"`) {
		t.Error("expected Check Status button to disable itself while checking")
	}
	if !strings.Contains(body, `Checking...`) {
		t.Error("expected Check Status button to include loading text")
	}
	if strings.Contains(body, `hx-target="#oauth-status-container"`) {
		t.Error("expected Check Status not to target the shared oauth status container")
	}
	if !strings.Contains(body, "Disconnect") {
		t.Error("expected Disconnect button when a Copilot token is stored")
	}
}

func TestOAuthStatus_CodexCheckStatusUsesHTMXWhenTokenExists(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	codexBackend := b.(*backend.CodexBackend)
	if err := codexBackend.GetTokenStore().Save(&oauth.TokenData{
		AccessToken:  "codex-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	}); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/oauth/status", nil)
	w := httptest.NewRecorder()

	ui.OAuthStatus(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `hx-post="/ui/oauth/check-status/codex"`) {
		t.Error("expected Codex Check Status button to point to the check-status endpoint")
	}
	if !strings.Contains(body, `hx-target="#oauth-status-codex"`) {
		t.Error("expected Codex Check Status button to target only the Codex card")
	}
	if !strings.Contains(body, `Checking...`) {
		t.Error("expected Codex Check Status button to include loading text")
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
func createTestUIWithTokenURL(t *testing.T, tokenURL string, clientID string) (*UI, func()) {
	t.Helper()
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")
	tokenDir := filepath.Join(tmpDir, "tokens")
	os.MkdirAll(tokenDir, 0700)

	var oauthConfig strings.Builder
	oauthConfig.WriteString("    oauth:\n")
	oauthConfig.WriteString(`      token_url: "` + tokenURL + "\"\n")
	if clientID != "" {
		oauthConfig.WriteString(`      client_id: "` + clientID + "\"\n")
	}

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
` + oauthConfig.String()
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

	ui := NewUI(cfgMgr, collector, registry, nil, nil, nil, nil)

	cleanup := func() {
		os.RemoveAll(tmpDir)
	}

	return ui, cleanup
}

func TestOAuthCallback_FullFlow(t *testing.T) {
	// Set up a mock token server that exchanges the code.
	var receivedCode string
	var receivedRedirectURI string
	tokenServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	ui, cleanup := createTestUIWithTokenURL(t, tokenServer.URL, "custom-client-id")
	defer cleanup()

	// Set up routes.
	r := chi.NewRouter()
	r.Get("/ui/oauth/login/{backend}", ui.OAuthLogin)
	r.Get("/ui/oauth/callback/{backend}", ui.OAuthCallback)

	loginServer := newTestServer(t, r)
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

	// Should redirect to models on success.
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect to models, got %d", resp.StatusCode)
	}

	finalLocation := resp.Header.Get("Location")
	if !strings.Contains(finalLocation, "/ui/models") {
		t.Errorf("expected redirect to /ui/models, got %s", finalLocation)
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

func TestOAuthCheckStatus_TriggersTokenRefreshForCopilot(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Create a mock GitHub API server that handles the Copilot token exchange
	mockGitHub := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/copilot_internal/v2/token" {
			// Return a fresh Copilot token
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "fresh-copilot-token",
				"expires_at": time.Now().Add(30 * time.Minute).Unix(),
				"refresh_in": 1500,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockGitHub.Close()

	// Create a mock store with an expired token + GitHub token
	tokenDir := filepath.Join(t.TempDir(), "tokens")
	os.MkdirAll(tokenDir, 0700)
	mockStore, _ := oauth.NewTokenStore(filepath.Join(tokenDir, "copilot-token.json"))
	mockStore.Save(&oauth.TokenData{
		AccessToken: "expired-copilot-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		GitHubToken: "test-github-token",
	})

	// Build a Copilot backend with the mock exchanger URL
	deviceCodeHandler := oauth.NewDeviceCodeHandler(mockStore,
		oauth.WithCopilotExchangerURL(mockGitHub.URL),
	)
	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
		Models:  []config.ModelConfig{{ID: "gpt-4o"}},
	}
	mockBackend := backend.NewCopilotBackend(cfg, deviceCodeHandler, mockStore)
	ui.registry.RegisterBackend("copilot", mockBackend)

	// Route through chi so URL params work
	r := chi.NewRouter()
	r.Post("/ui/oauth/check-status/{backend}", ui.OAuthCheckStatus)
	server := newTestServer(t, r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/ui/oauth/check-status/copilot", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, string(body))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)

	// The response should show Connected status with the fresh token
	if !strings.Contains(body, "Connected") {
		t.Errorf("expected 'Connected' in response body, got: %s", body)
	}
	if strings.Contains(body, `id="oauth-status-container"`) {
		t.Error("expected single-card response, not the full oauth status container")
	}
	if strings.Contains(body, "codex") {
		t.Error("expected Copilot check-status response not to include the Codex card")
	}

	// Verify the token store now has the fresh token
	newToken := mockStore.Get()
	if newToken == nil {
		t.Fatal("expected token to be stored after check status")
	}
	if newToken.AccessToken != "fresh-copilot-token" {
		t.Errorf("access token = %q, want %q", newToken.AccessToken, "fresh-copilot-token")
	}
}

func TestOAuthCheckStatus_TriggersTokenRefreshForCodex(t *testing.T) {
	refreshCalls := 0
	tokenServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parsing form: %v", err)
		}
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", r.Form.Get("grant_type"))
		}
		refreshCalls++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fresh-codex-token",
			"refresh_token": "rotated-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	ui, cleanup := createTestUIWithTokenURL(t, tokenServer.URL, "custom-client-id")
	defer cleanup()

	b := ui.registry.Get("codex")
	if b == nil {
		t.Fatal("codex backend not found")
	}
	codexBackend := b.(*backend.CodexBackend)
	store := codexBackend.GetTokenStore()
	if err := store.Save(&oauth.TokenData{
		AccessToken:  "expired-codex-token",
		RefreshToken: "refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:       "codex_oauth",
	}); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	r := chi.NewRouter()
	r.Post("/ui/oauth/check-status/{backend}", ui.OAuthCheckStatus)
	server := newTestServer(t, r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/ui/oauth/check-status/codex", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200, got %d: %s", resp.StatusCode, string(body))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	if !strings.Contains(body, "Connected") {
		t.Errorf("expected connected Codex status after refresh, got: %s", body)
	}
	if strings.Contains(body, "copilot") {
		t.Error("expected Codex check-status response not to include the Copilot card")
	}
	if refreshCalls != 1 {
		t.Fatalf("expected 1 Codex refresh call, got %d", refreshCalls)
	}

	newToken := store.Get()
	if newToken == nil {
		t.Fatal("expected Codex token to be stored after check status")
	}
	if newToken.AccessToken != "fresh-codex-token" {
		t.Errorf("access token = %q, want %q", newToken.AccessToken, "fresh-codex-token")
	}
}

func TestOAuthCheckStatus_ReturnsModelsCardFragmentWhenRequested(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	b := ui.registry.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend not found")
	}
	copilotBackend := b.(*backend.CopilotBackend)
	if err := copilotBackend.GetTokenStore().Save(&oauth.TokenData{
		AccessToken: "copilot-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(1 * time.Hour),
		ObtainedAt:  time.Now(),
		Source:      "device_code_flow",
	}); err != nil {
		t.Fatalf("saving token: %v", err)
	}

	r := chi.NewRouter()
	r.Post("/ui/oauth/check-status/{backend}", ui.OAuthCheckStatus)
	server := newTestServer(t, r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/ui/oauth/check-status/copilot?view=models", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	if !strings.Contains(body, `id="oauth-copilot"`) {
		t.Error("expected models oauth card fragment id")
	}
	if strings.Contains(body, `id="oauth-status-copilot"`) {
		t.Error("expected models fragment, not standalone oauth status card")
	}
}

func TestOAuthCheckStatus_Returns404ForUnknownBackend(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	r := chi.NewRouter()
	r.Post("/ui/oauth/check-status/{backend}", ui.OAuthCheckStatus)
	server := newTestServer(t, r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/ui/oauth/check-status/nonexistent", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", resp.StatusCode)
	}
}

func TestOAuthCheckStatus_ReturnsStatusEvenOnRefreshFailure(t *testing.T) {
	ui, cleanup := createTestUI(t)
	defer cleanup()

	// Store an expired token with no GitHub token (refresh will fail)
	b := ui.registry.Get("copilot")
	if b == nil {
		t.Fatal("copilot backend not found")
	}
	copilotBackend := b.(*backend.CopilotBackend)
	ts := copilotBackend.GetTokenStore()

	ts.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		// No GitHubToken — re-exchange will fail
	})

	// Route through chi so URL params work
	r := chi.NewRouter()
	r.Post("/ui/oauth/check-status/{backend}", ui.OAuthCheckStatus)
	server := newTestServer(t, r)
	defer server.Close()

	resp, err := http.Post(server.URL+"/ui/oauth/check-status/copilot", "", nil)
	if err != nil {
		t.Fatalf("making request: %v", err)
	}
	defer resp.Body.Close()

	// Should still return 200 with the status fragment (showing error state)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected status 200 (graceful degradation), got %d: %s", resp.StatusCode, string(body))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	body := string(bodyBytes)
	// Should still render the status card
	if !strings.Contains(body, "copilot") {
		t.Error("expected copilot backend card in response even on refresh failure")
	}
	// Should show not connected or expired state
	if !strings.Contains(body, "Expired") && !strings.Contains(body, "Not connected") {
		t.Error("expected error status indicator in response")
	}
}
