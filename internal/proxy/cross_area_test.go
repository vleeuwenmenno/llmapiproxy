package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/stats"
)

// ============================================================================
// Cross-Area Integration Tests
// These tests exercise multiple components (registry, handler, backends,
// config, stats) together to validate end-to-end cross-area flows.
// ============================================================================


// copilotTestEnv sets up a Copilot backend with mock GitHub API and mock
// Copilot upstream. Returns backend, cleanup function, and upstream tracker.
// It sets COPILOT_GITHUB_TOKEN env var for token discovery.
func copilotTestEnv(t *testing.T, opts ...func(*copilotTestOpts)) (*backend.CopilotBackend, func(), *copilotTracker) {
	t.Helper()
	o := &copilotTestOpts{
		copilotToken: "copilot-test-token",
		upstreamResp: map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello from Copilot!"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		},
		upstreamStatus: 200,
	}
	for _, opt := range opts {
		opt(o)
	}

	tracker := &copilotTracker{}

	// Mock GitHub token exchange server.
	githubAPI := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracker.mu.Lock()
		tracker.githubExchangeCount++
		tracker.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      o.copilotToken,
		})
	}))

	// Mock Copilot upstream API.
	copilotUpstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id": "gpt-4o", "object": "model", "owned_by": "copilot",
						"capabilities": map[string]any{
							"type":     "chat",
							"supports": map[string]any{"streaming": true},
							"limits":   map[string]any{"max_output_tokens": 16384},
						},
					},
					{
						"id": "gpt-4.1", "object": "model", "owned_by": "copilot",
						"capabilities": map[string]any{
							"type":     "chat",
							"supports": map[string]any{"streaming": true},
							"limits":   map[string]any{"max_output_tokens": 32768},
						},
					},
				},
			})
			return
		}

		tracker.mu.Lock()
		tracker.upstreamRequestCount++
		tracker.lastAuth = r.Header.Get("Authorization")
		tracker.lastEditorVersion = r.Header.Get("Editor-Version")
		tracker.lastIntegrationID = r.Header.Get("Copilot-Integration-Id")
		tracker.lastUserAgent = r.Header.Get("User-Agent")
		tracker.lastRequestID = r.Header.Get("X-Request-Id")
		tracker.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if o.upstreamStatus != 200 {
			w.WriteHeader(o.upstreamStatus)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"message": "upstream error"},
			})
			return
		}
		json.NewEncoder(w).Encode(o.upstreamResp)
	}))

	// Create temp token store.
	tempDir, err := os.MkdirTemp("", "copilot-cross-test-*")
	if err != nil {
		t.Fatal(err)
	}
	tokenStore, tsErr := oauth.NewTokenStore(filepath.Join(tempDir, "copilot-token.json"))
	if tsErr != nil {
		t.Fatal(tsErr)
	}

	// Pre-seed the token store so the backend doesn't need env vars for discovery.
	// This makes tests deterministic.
	tokenStore.Save(&oauth.TokenData{
		AccessToken: o.copilotToken,
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	// Use device code handler with mock exchange URL.
	deviceCodeHandler := oauth.NewDeviceCodeHandler(tokenStore, oauth.WithCopilotExchangerURL(githubAPI.URL))

	b := backend.NewCopilotBackend(
		config.BackendConfig{
			Name:    "copilot",
			Type:    "copilot",
			BaseURL: copilotUpstream.URL,
			Models:  o.models,
		},
		deviceCodeHandler, tokenStore,
	)

	cleanup := func() {
		githubAPI.Close()
		copilotUpstream.Close()
		os.RemoveAll(tempDir)
	}

	return b, cleanup, tracker
}

type copilotTestOpts struct {
	copilotToken   string
	upstreamResp   map[string]any
	upstreamStatus int
	models         []config.ModelConfig
}

type copilotTracker struct {
	mu                   sync.Mutex
	githubExchangeCount  int
	upstreamRequestCount int
	lastAuth             string
	lastEditorVersion    string
	lastIntegrationID    string
	lastUserAgent        string
	lastRequestID        string
}

// openaiTestEnv creates a mock OpenAI upstream server for testing mixed routing.
func openaiTestEnv(t *testing.T) (backend.Backend, func()) {
	t.Helper()

	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-openai-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "openai/gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello from OpenAI!"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12},
		})
	}))

	b := backend.NewOpenAI(config.BackendConfig{
		Name:    "openrouter",
		Type:    "openai",
		BaseURL: upstream.URL,
		APIKey:  "test-or-key",
		Models:  []config.ModelConfig{{ID: "openai/gpt-4o"}, {ID: "anthropic/claude-sonnet-4"}},
	}, 0)

	return b, upstream.Close
}

// --- VAL-CROSS-001: First-visit OAuth setup — Copilot backend from cold start ---

func TestCrossArea_CopilotColdStart(t *testing.T) {
	copilotB, cleanup, tracker := copilotTestEnv(t)
	defer cleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)

	if !registry.Has("copilot") {
		t.Fatal("copilot backend should be registered")
	}

	models, err := copilotB.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected at least one model from Copilot backend")
	}

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	resp, err := copilotB.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion error: %v", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		t.Fatal("expected non-empty choices in response")
	}
	if string(resp.Choices[0].Message.Content) != "\"Hello from Copilot!\"" {
		t.Errorf("response content = %q, want %q", resp.Choices[0].Message.Content, "Hello from Copilot!")
	}

	// Verify upstream was called with proper Copilot headers.
	tracker.mu.Lock()
	count := tracker.upstreamRequestCount
	auth := tracker.lastAuth
	edVer := tracker.lastEditorVersion
	intID := tracker.lastIntegrationID
	ua := tracker.lastUserAgent
	reqID := tracker.lastRequestID
	tracker.mu.Unlock()

	if count == 0 {
		t.Error("expected at least one upstream request")
	}
	if auth != "Bearer copilot-test-token" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer copilot-test-token")
	}
	if edVer == "" {
		t.Error("Editor-Version header should be set")
	}
	if intID == "" {
		t.Error("Copilot-Integration-Id header should be set")
	}
	if ua == "" {
		t.Error("User-Agent header should be set")
	}
	if reqID == "" {
		t.Error("X-Request-Id header should be set")
	}
}

// --- VAL-CROSS-003: Token refresh during active use ---

func TestCrossArea_CopilotTokenRefreshDuringUse(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}

	// Three sequential requests — all should succeed using cached token.
	for i := 0; i < 3; i++ {
		resp, err := copilotB.ChatCompletion(context.Background(), req)
		if err != nil {
			t.Fatalf("request %d error: %v", i+1, err)
		}
		if string(resp.Choices[0].Message.Content) != "\"Hello from Copilot!\"" {
			t.Errorf("request %d response = %q", i+1, resp.Choices[0].Message.Content)
		}
	}
}

// --- VAL-CROSS-004: Mixed backend routing ---

func TestCrossArea_MixedBackendRouting(t *testing.T) {
	copilotB, copCleanup, _ := copilotTestEnv(t)
	defer copCleanup()

	openaiB, oaiCleanup := openaiTestEnv(t)
	defer oaiCleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)
	registry.Register("openrouter", openaiB)

	cfgMgr, cfgCleanup := newTestConfigMgr(t)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test"})

	// Request to Copilot backend.
	copReq := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
	copReq = copReq.WithContext(ctx)
	copRec := httptest.NewRecorder()
	handler.ChatCompletions(copRec, copReq)
	if copRec.Code != http.StatusOK {
		t.Fatalf("copilot request status = %d, want %d", copRec.Code, http.StatusOK)
	}

	// Request to OpenAI backend.
	oaiReq := makeChatRequest(t, "openrouter/openai/gpt-4o", false, "test-api-key")
	oaiReq = oaiReq.WithContext(ctx)
	oaiRec := httptest.NewRecorder()
	handler.ChatCompletions(oaiRec, oaiReq)
	if oaiRec.Code != http.StatusOK {
		t.Fatalf("openrouter request status = %d, want %d", oaiRec.Code, http.StatusOK)
	}

	// Verify both backends appear in model listing.
	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsRec := httptest.NewRecorder()
	handler.ListModels(modelsRec, modelsReq)

	var modelList backend.ModelList
	if err := json.NewDecoder(modelsRec.Body).Decode(&modelList); err != nil {
		t.Fatalf("decode models: %v", err)
	}

	hasCopilot, hasOpenRouter := false, false
	for _, m := range modelList.Data {
		if strings.HasPrefix(m.ID, "copilot/") {
			hasCopilot = true
		}
		if strings.HasPrefix(m.ID, "openrouter/") {
			hasOpenRouter = true
		}
	}
	if !hasCopilot {
		t.Error("expected copilot models in model listing")
	}
	if !hasOpenRouter {
		t.Error("expected openrouter models in model listing")
	}

	// Verify stats for both backends.
	records := collector.Recent(10)
	if len(records) < 2 {
		t.Fatalf("expected at least 2 stats records, got %d", len(records))
	}
	backendNames := make(map[string]bool)
	for _, r := range records {
		backendNames[r.Backend] = true
	}
	if !backendNames["copilot"] {
		t.Error("expected stats for copilot")
	}
	if !backendNames["openrouter"] {
		t.Error("expected stats for openrouter")
	}
}

// --- VAL-CROSS-005: Failover with OAuth backend ---

func TestCrossArea_FailoverWithOAuthBackend(t *testing.T) {
	failingCopilot := &mockBackend{
		name:   "copilot",
		models: []string{"gpt-4o"},
		chatErr: &backend.BackendError{
			StatusCode: http.StatusInternalServerError,
			Body:       `{"error":{"message":"internal server error"}}`,
			Err:        fmt.Errorf("copilot returned 500"),
		},
	}

	openaiB, oaiCleanup := openaiTestEnv(t)
	defer oaiCleanup()

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "copilot/gpt-4o", Backends: []string{"copilot", "openrouter"}},
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"copilot":    failingCopilot,
		"openrouter": openaiB,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test"})
	req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("failover status = %d, want %d", rec.Code, http.StatusOK)
	}

	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records")
	}
	if records[0].Backend != "openrouter" {
		t.Errorf("stats backend = %q, want %q", records[0].Backend, "openrouter")
	}
}

// --- VAL-CROSS-006: SIGHUP preserves token state ---

func TestCrossArea_SIGHUPPreservesTokenState(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	_, err := copilotB.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("initial request error: %v", err)
	}

	status := copilotB.OAuthStatus()
	if !status.Authenticated {
		t.Error("expected Copilot to be authenticated after request")
	}

	ts := copilotB.GetTokenStore()
	token := ts.Get()
	if token == nil {
		t.Fatal("expected token to be stored")
	}
	originalToken := token.AccessToken
	originalExpiry := token.ExpiresAt

	// Simulate reload by re-registering with the same backend (same token store).
	registry2 := backend.NewRegistry()
	registry2.Register("copilot", copilotB)

	// Token still present.
	token2 := ts.Get()
	if token2 == nil {
		t.Fatal("expected token to persist across reload")
	}
	if token2.AccessToken != originalToken {
		t.Errorf("token mismatch after reload: got %q, want %q", token2.AccessToken, originalToken)
	}
	if !token2.ExpiresAt.Equal(originalExpiry) {
		t.Errorf("expiry mismatch after reload")
	}

	// Backend still works.
	resp, err := copilotB.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("post-reload request error: %v", err)
	}
	if string(resp.Choices[0].Message.Content) != "\"Hello from Copilot!\"" {
		t.Errorf("post-reload response = %q", resp.Choices[0].Message.Content)
	}
}

// --- VAL-CROSS-007: Regression — existing functionality preserved ---

func TestCrossArea_Regression_ExistingFunctionality(t *testing.T) {
	openaiB, oaiCleanup := openaiTestEnv(t)
	defer oaiCleanup()

	copilotB, copCleanup, _ := copilotTestEnv(t)
	defer copCleanup()

	registry := backend.NewRegistry()
	registry.Register("openrouter", openaiB)
	registry.Register("copilot", copilotB)

	cfgMgr, cfgCleanup := newTestConfigMgr(t)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test"})

	t.Run("OpenAI backend works", func(t *testing.T) {
		req := makeChatRequest(t, "openrouter/openai/gpt-4o", false, "test-api-key")
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ChatCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("OpenAI status = %d", rec.Code)
		}
	})

	t.Run("Model listing works", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler.ListModels(rec, req)
		var ml backend.ModelList
		if err := json.NewDecoder(rec.Body).Decode(&ml); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ml.Object != "list" || len(ml.Data) == 0 {
			t.Errorf("unexpected model list: object=%q count=%d", ml.Object, len(ml.Data))
		}
	})

	t.Run("Stats recording works", func(t *testing.T) {
		records := collector.Recent(10)
		if len(records) == 0 {
			t.Fatal("expected stats records")
		}
	})

	t.Run("Copilot alongside OpenAI", func(t *testing.T) {
		req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ChatCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Copilot status = %d", rec.Code)
		}
	})
}

// --- VAL-CROSS-008: Token persistence across restarts ---

func TestCrossArea_TokenPersistenceAcrossRestarts(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "token-persist-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	tokenPath := filepath.Join(tempDir, "copilot-token.json")

	githubAPI := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      "copilot-persist-test-token",
		})
	}))
	defer githubAPI.Close()

	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7},
		})
	}))
	defer upstream.Close()

	// --- First instance: save a token ---
	ts1, err := oauth.NewTokenStore(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	ts1.Save(&oauth.TokenData{
		AccessToken: "copilot-persist-test-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test:persistence",
	})

	dch1 := oauth.NewDeviceCodeHandler(ts1, oauth.WithCopilotExchangerURL(githubAPI.URL))
	b1 := backend.NewCopilotBackend(
		config.BackendConfig{Name: "copilot", Type: "copilot", BaseURL: upstream.URL},
		dch1, ts1,
	)

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	resp, err := b1.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("first instance error: %v", err)
	}
	if string(resp.Choices[0].Message.Content) != "\"Hello!\"" {
		t.Errorf("first response = %q", resp.Choices[0].Message.Content)
	}

	// Verify token file on disk.
	if _, err := os.Stat(tokenPath); os.IsNotExist(err) {
		t.Fatal("token file should exist on disk")
	}

	// --- Simulate restart: load token from same path ---
	ts2, err := oauth.NewTokenStore(tokenPath)
	if err != nil {
		t.Fatal(err)
	}

	token := ts2.Get()
	if token == nil {
		t.Fatal("expected token loaded from disk")
	}
	if token.AccessToken != "copilot-persist-test-token" {
		t.Errorf("loaded token = %q, want %q", token.AccessToken, "copilot-persist-test-token")
	}
	if token.Source != "test:persistence" {
		t.Errorf("loaded source = %q, want %q", token.Source, "test:persistence")
	}

	dch2 := oauth.NewDeviceCodeHandler(ts2, oauth.WithCopilotExchangerURL(githubAPI.URL))
	b2 := backend.NewCopilotBackend(
		config.BackendConfig{Name: "copilot", Type: "copilot", BaseURL: upstream.URL},
		dch2, ts2,
	)

	resp2, err := b2.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("restarted instance error: %v", err)
	}
	if string(resp2.Choices[0].Message.Content) != "\"Hello!\"" {
		t.Errorf("restarted response = %q", resp2.Choices[0].Message.Content)
	}
}

// --- VAL-CROSS-009: Playground with OAuth backends ---

func TestCrossArea_PlaygroundWithOAuthBackends(t *testing.T) {
	copilotB, copCleanup, _ := copilotTestEnv(t)
	defer copCleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)

	cfgMgr, cfgCleanup := newTestConfigMgr(t)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	handler.ListModels(rec, req)

	var ml backend.ModelList
	if err := json.NewDecoder(rec.Body).Decode(&ml); err != nil {
		t.Fatalf("decode: %v", err)
	}

	hasCopilotModels := false
	for _, m := range ml.Data {
		if strings.HasPrefix(m.ID, "copilot/") {
			hasCopilotModels = true
			if m.Object != "model" {
				t.Errorf("object = %q, want %q", m.Object, "model")
			}
			if m.OwnedBy != "copilot" {
				t.Errorf("owned_by = %q, want %q", m.OwnedBy, "copilot")
			}
		}
	}
	if !hasCopilotModels {
		t.Error("expected Copilot models in listing")
	}
}

// --- VAL-CROSS-010: Error path — no GitHub token available ---

func TestCrossArea_NoGitHubToken(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "no-token-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	ts, err := oauth.NewTokenStore(filepath.Join(tempDir, "copilot-token.json"))
	if err != nil {
		t.Fatal(err)
	}

	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream should not be called")
	}))
	defer upstream.Close()

	// Device code handler with no token (no device code flow completed).
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := backend.NewCopilotBackend(
		config.BackendConfig{Name: "copilot", Type: "copilot", BaseURL: upstream.URL},
		deviceCodeHandler, ts,
	)

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	_, err = b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no GitHub token available")
	}

	errMsg := err.Error()
	if !strings.Contains(strings.ToLower(errMsg), "token") {
		t.Errorf("error should mention token: %q", errMsg)
	}

	status := b.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated")
	}
	if status.TokenState != "missing" {
		t.Errorf("token state = %q, want %q", status.TokenState, "missing")
	}
}

// --- VAL-CROSS-011: OAuth callback error handling ---

func TestCrossArea_OAuthCallbackErrors(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "callback-error-test-*")
	defer os.RemoveAll(tempDir)

	ts, _ := oauth.NewTokenStore(filepath.Join(tempDir, "codex-token.json"))

	// Mock OAuth server that returns error on token exchange.
	oauthServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "The authorization code is invalid or expired.",
		})
	}))
	defer oauthServer.Close()

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthCfg.TokenURL = oauthServer.URL

	handler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	_, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("authorize URL error: %v", err)
	}

	_, err = handler.HandleCallback(context.Background(), "invalid-code", state)
	if err == nil {
		t.Fatal("expected error for invalid authorization code")
	}
	if !strings.Contains(err.Error(), "invalid") && !strings.Contains(err.Error(), "400") {
		t.Errorf("error should mention invalid or 400: %q", err.Error())
	}

	// No token stored after failure.
	if token := ts.Get(); token != nil {
		t.Error("no token should be stored after failed callback")
	}
}

// --- VAL-CROSS-012: OAuth token expiry with no refresh ---

func TestCrossArea_OAuthTokenExpiry_NoRefresh(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "token-expiry-test-*")
	defer os.RemoveAll(tempDir)

	ts, _ := oauth.NewTokenStore(filepath.Join(tempDir, "codex-token.json"))
	ts.Save(&oauth.TokenData{
		AccessToken:  "expired-codex-token",
		RefreshToken: "",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:       "codex_oauth",
	})

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)
	b := backend.NewCodexBackend(
		config.BackendConfig{
			Name: "codex", Type: "codex", BaseURL: "https://chatgpt.com/backend-api/codex",
			Models: []config.ModelConfig{{ID: "o4-mini"}},
		},
		oauthHandler, ts, nil)

	status := b.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated with expired token")
	}
	if status.TokenState != "expired" {
		t.Errorf("token state = %q, want %q", status.TokenState, "expired")
	}
	if !status.NeedsReauth {
		t.Error("should need reauth when expired with no refresh token")
	}

	req := &backend.ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for expired token with no refresh")
	}
}

// --- VAL-CROSS-013: Simultaneous OAuth flows ---

func TestCrossArea_SimultaneousOAuthFlows(t *testing.T) {
	copilotB, copCleanup, _ := copilotTestEnv(t)
	defer copCleanup()

	tempDir, _ := os.MkdirTemp("", "dual-oauth-test-*")
	defer os.RemoveAll(tempDir)

	codexTS, _ := oauth.NewTokenStore(filepath.Join(tempDir, "codex-token.json"))
	codexTS.Save(&oauth.TokenData{
		AccessToken:  "valid-codex-token",
		RefreshToken: "valid-refresh-token",
		ExpiresAt:    time.Now().Add(30 * time.Minute),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	})

	codexUpstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-test",
			"object": "response",
			"status": "completed",
			"model":  "o4-mini",
			"output": []map[string]any{},
			"usage":  map[string]any{"input_tokens": 5, "output_tokens": 3},
		})
	}))
	defer codexUpstream.Close()

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	codexHandler := oauth.NewCodexOAuthHandler(codexTS, oauthCfg)
	codexB := backend.NewCodexBackend(
		config.BackendConfig{
			Name: "codex", Type: "codex", BaseURL: codexUpstream.URL,
			Models: []config.ModelConfig{{ID: "o4-mini"}},
		},
		codexHandler, codexTS, nil)

	// Both report independent OAuth statuses.
	if copilotB.OAuthStatus().BackendName != "copilot" {
		t.Error("copilot backend name mismatch")
	}
	if codexB.OAuthStatus().BackendName != "codex" {
		t.Error("codex backend name mismatch")
	}

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)
	registry.Register("codex", codexB)

	statuses := registry.OAuthStatuses()
	if len(statuses) != 2 {
		t.Errorf("expected 2 OAuth statuses, got %d", len(statuses))
	}
}

// --- VAL-CROSS-014: Stats tracking for OAuth backends ---

func TestCrossArea_StatsTrackingForOAuthBackends(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)

	cfgMgr, cfgCleanup := newTestConfigMgr(t)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "stats-client"})
	for i := 0; i < 3; i++ {
		req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		handler.ChatCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d", i, rec.Code)
		}
	}

	records := collector.Recent(10)
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}

	for i, r := range records {
		if r.Backend != "copilot" {
			t.Errorf("record %d: backend = %q", i, r.Backend)
		}
		if r.Model != "copilot/gpt-4o" {
			t.Errorf("record %d: model = %q", i, r.Model)
		}
		if r.StatusCode != http.StatusOK {
			t.Errorf("record %d: status = %d", i, r.StatusCode)
		}
	}

	summary := collector.Summarize(0)
	if summary.TotalRequests != 3 {
		t.Errorf("total = %d, want 3", summary.TotalRequests)
	}
	if summary.ByBackend["copilot"] != 3 {
		t.Errorf("copilot count = %d, want 3", summary.ByBackend["copilot"])
	}
}

// --- VAL-CROSS-015: Backend toggling with OAuth ---

func TestCrossArea_BackendTogglingWithOAuth(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)
	if !registry.Has("copilot") {
		t.Fatal("copilot should be registered")
	}

	// Simulate disable: new registry without copilot.
	registry2 := backend.NewRegistry()
	_, _, err := registry2.Resolve("copilot/gpt-4o")
	if err == nil {
		t.Error("expected error for unregistered backend")
	}
	if !strings.Contains(err.Error(), "no backend found") {
		t.Errorf("error = %q", err.Error())
	}

	// Re-enable.
	registry2.Register("copilot", copilotB)
	b, modelID, err := registry2.Resolve("copilot/gpt-4o")
	if err != nil {
		t.Fatalf("re-enabled resolve error: %v", err)
	}
	if b.Name() != "copilot" {
		t.Errorf("backend = %q", b.Name())
	}
	if modelID != "gpt-4o" {
		t.Errorf("model = %q", modelID)
	}
}

// --- VAL-CROSS-016: Config validation with OAuth backends ---

func TestCrossArea_ConfigValidation_OAuthMisconfiguration(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "Copilot without base_url",
			yaml: `
server:
  listen: ":8000"
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: ""
`,
			wantErr: "base_url",
		},
		{
			name: "OpenAI without api_key",
			yaml: `
server:
  listen: ":8000"
  api_keys: ["test-key"]
backends:
  - name: openrouter
    type: openai
    base_url: "https://openrouter.ai/api/v1"
    api_key: ""
`,
			wantErr: "api_key",
		},
		{
			name: "Valid Copilot config",
			yaml: `
server:
  listen: ":8000"
  api_keys: ["test-key"]
backends:
  - name: copilot
    type: copilot
    base_url: "https://api.githubcopilot.com"
`,
			wantErr: "",
		},
		{
			name: "Valid Codex config",
			yaml: `
server:
  listen: ":8000"
  api_keys: ["test-key"]
backends:
  - name: codex
    type: codex
    base_url: "https://chatgpt.com/backend-api/codex"
    oauth:
      client_id: "test-client-id"
`,
			wantErr: "",
		},
		{
			name: "Unknown backend type passes validation",
			yaml: `
server:
  listen: ":8000"
  api_keys: ["test-key"]
backends:
  - name: unknown
    type: unknown
    base_url: "https://example.com"
    api_key: "some-key"
`,
			wantErr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.yaml))
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			} else {
				if err == nil {
					t.Errorf("expected error containing %q", tt.wantErr)
				} else if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want %q", err.Error(), tt.wantErr)
				}
			}
		})
	}
}

// --- VAL-CROSS-017: OAuth streaming ---

func TestCrossArea_OAuthStreaming(t *testing.T) {
	sseUpstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		chunks := []string{
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" from"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-stream","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" Copilot!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		}
		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			flusher.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer sseUpstream.Close()

	tempDir, _ := os.MkdirTemp("", "stream-test-*")
	defer os.RemoveAll(tempDir)

	ts, _ := oauth.NewTokenStore(filepath.Join(tempDir, "copilot-token.json"))
	ts.Save(&oauth.TokenData{
		AccessToken: "test-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	dch := oauth.NewDeviceCodeHandler(ts)
	b := backend.NewCopilotBackend(
		config.BackendConfig{Name: "copilot", Type: "copilot", BaseURL: sseUpstream.URL},
		dch, ts,
	)

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
		Stream:   true,
	}

	stream, err := b.ChatCompletionStream(context.Background(), req)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()

	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}

	sseBody := string(body)
	if !strings.Contains(sseBody, "data: ") {
		t.Error("expected SSE data lines")
	}
	if !strings.Contains(sseBody, "[DONE]") {
		t.Error("expected [DONE] sentinel")
	}
}

// --- VAL-CROSS-018: Multiple clients with different OAuth backends ---

func TestCrossArea_MultipleClientsWithOAuthBackends(t *testing.T) {
	copilotB, copCleanup, _ := copilotTestEnv(t)
	defer copCleanup()

	openaiB, oaiCleanup := openaiTestEnv(t)
	defer oaiCleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)
	registry.Register("openrouter", openaiB)

	clientA := config.ClientConfig{Name: "client-a", APIKey: "client-a-key"}
	clientB := config.ClientConfig{Name: "client-b", APIKey: "client-b-key"}

	cfgMgr, cfgCleanup := newTestConfigMgr(t, clientA, clientB)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	// Client A → Copilot.
	ctxA := context.WithValue(context.Background(), clientContextKey{}, &clientA)
	reqA := makeChatRequest(t, "copilot/gpt-4o", false, "client-a-key")
	reqA = reqA.WithContext(ctxA)
	recA := httptest.NewRecorder()
	handler.ChatCompletions(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("client A status = %d", recA.Code)
	}

	// Client B → OpenRouter.
	ctxB := context.WithValue(context.Background(), clientContextKey{}, &clientB)
	reqB := makeChatRequest(t, "openrouter/openai/gpt-4o", false, "client-b-key")
	reqB = reqB.WithContext(ctxB)
	recB := httptest.NewRecorder()
	handler.ChatCompletions(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("client B status = %d", recB.Code)
	}

	records := collector.Recent(10)
	clientMap := make(map[string]string)
	for _, r := range records {
		clientMap[r.Client] = r.Backend
	}
	if clientMap["client-a"] != "copilot" {
		t.Errorf("client-a → %q, want copilot", clientMap["client-a"])
	}
	if clientMap["client-b"] != "openrouter" {
		t.Errorf("client-b → %q, want openrouter", clientMap["client-b"])
	}
}

// --- VAL-CROSS-019: Health check reflects OAuth backend status ---

func TestCrossArea_HealthCheckReflectsOAuthStatus(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)

	// Force a request to populate token.
	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	_, _ = copilotB.ChatCompletion(context.Background(), req)

	status := copilotB.OAuthStatus()
	if !status.Authenticated {
		t.Error("expected authenticated after request")
	}
	if status.TokenState != "valid" {
		t.Errorf("token state = %q, want %q", status.TokenState, "valid")
	}

	// Test expired token scenario.
	tempDir, _ := os.MkdirTemp("", "health-test-*")
	defer os.RemoveAll(tempDir)

	expiredTS, _ := oauth.NewTokenStore(filepath.Join(tempDir, "expired-token.json"))
	expiredTS.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "test",
	})

	expiredB := backend.NewCopilotBackend(
		config.BackendConfig{Name: "expired-copilot", Type: "copilot", BaseURL: "https://api.githubcopilot.com"},
		oauth.NewDeviceCodeHandler(expiredTS), expiredTS,
	)

	expiredStatus := expiredB.OAuthStatus()
	if expiredStatus.Authenticated {
		t.Error("expired should not be authenticated")
	}
	if expiredStatus.TokenState != "expired" {
		t.Errorf("expired state = %q, want %q", expiredStatus.TokenState, "expired")
	}

	// Registry OAuthStatuses works.
	statuses := registry.OAuthStatuses()
	if len(statuses) == 0 {
		t.Error("expected OAuth statuses from registry")
	}
}

// --- VAL-CROSS-020: OAuth disconnect and re-auth ---

func TestCrossArea_OAuthDisconnectAndReauth(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	req := &backend.ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []backend.Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
	}
	_, err := copilotB.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("initial error: %v", err)
	}

	status := copilotB.OAuthStatus()
	if !status.Authenticated {
		t.Fatal("expected authenticated before disconnect")
	}

	// Disconnect.
	if err := copilotB.Disconnect(); err != nil {
		t.Fatalf("disconnect error: %v", err)
	}

	status = copilotB.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated after disconnect")
	}
	if status.TokenState != "missing" {
		t.Errorf("state = %q, want %q", status.TokenState, "missing")
	}

	// With device code flow, after disconnect, the user needs to re-initiate
	// the device code flow via the web UI. Automatic re-auth is not possible
	// since the GitHub token is also cleared.
	// This is the expected behavior — the old discoverer-based flow could
	// re-discover from env vars, but device code flow requires explicit re-auth.
	_, err = copilotB.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Error("expected error after disconnect (device code flow requires re-auth via UI)")
	}

	// Verify disconnect clears everything (no stale tokens).
	status = copilotB.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated after failed re-auth")
	}
	if !status.NeedsReauth {
		t.Error("NeedsReauth should be true after disconnect")
	}
}

// --- VAL-CODEX-010 & VAL-CODEX-011: Codex OAuth PKCE flow ---

func TestCrossArea_CodexOAuthPKCEFlow(t *testing.T) {
	tempDir, _ := os.MkdirTemp("", "codex-pkce-test-*")
	defer os.RemoveAll(tempDir)

	ts, _ := oauth.NewTokenStore(filepath.Join(tempDir, "codex-token.json"))

	oauthServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email",
		})
	}))
	defer oauthServer.Close()

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthCfg.AuthURL = oauthServer.URL + "/authorize"
	oauthCfg.TokenURL = oauthServer.URL + "/token"

	handler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	// Step 1: Initiate login.
	authURL, state, err := handler.AuthorizeURL()
	if err != nil {
		t.Fatalf("authorize URL error: %v", err)
	}

	if !strings.Contains(authURL, "code_challenge") {
		t.Error("auth URL should contain code_challenge")
	}
	if !strings.Contains(authURL, "code_challenge_method=S256") {
		t.Error("auth URL should contain S256")
	}
	if !strings.Contains(authURL, "response_type=code") {
		t.Error("auth URL should contain response_type=code")
	}
	if state == "" {
		t.Error("state should not be empty")
	}

	// Step 2: Handle callback.
	tokenData, err := handler.HandleCallback(context.Background(), "test-auth-code", state)
	if err != nil {
		t.Fatalf("callback error: %v", err)
	}

	if tokenData.AccessToken != "test-access-token" {
		t.Errorf("access token = %q", tokenData.AccessToken)
	}
	if tokenData.RefreshToken != "test-refresh-token" {
		t.Errorf("refresh token = %q", tokenData.RefreshToken)
	}

	// Token persisted.
	stored := ts.Get()
	if stored == nil || stored.AccessToken != "test-access-token" {
		t.Error("token should be persisted")
	}

	// Invalid state rejected.
	_, err = handler.HandleCallback(context.Background(), "test-code", "invalid-state")
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
}

// --- Concurrent requests with OAuth backends ---

func TestCrossArea_ConcurrentRequests(t *testing.T) {
	copilotB, cleanup, _ := copilotTestEnv(t)
	defer cleanup()

	registry := backend.NewRegistry()
	registry.Register("copilot", copilotB)

	cfgMgr, cfgCleanup := newTestConfigMgr(t)
	defer cfgCleanup()

	collector := stats.NewCollector(1000)
	handler := NewHandler(registry, collector, cfgMgr)

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "concurrent"})

	var wg sync.WaitGroup
	errCh := make(chan error, 5)

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()
			handler.ChatCompletions(rec, req)
			if rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("request %d: status %d", idx, rec.Code)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent error: %v", err)
	}

	if len(collector.Recent(10)) != 5 {
		t.Errorf("expected 5 records, got %d", len(collector.Recent(10)))
	}
}

// --- VAL-CROSS-022: ListModels mode=raw vs mode=flat ---

func TestCrossArea_ListModelsRawMode(t *testing.T) {
	b1 := &mockBackend{
		name:   "openrouter",
		models: []string{"gpt-4o", "claude-sonnet-4"},
	}
	b2 := &mockBackend{
		name:   "copilot",
		models: []string{"gpt-4o"},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"openrouter": b1,
		"copilot":    b2,
	}, config.RoutingConfig{})
	defer cleanup()

	t.Run("default mode returns flattened models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		rec := httptest.NewRecorder()
		handler.ListModels(rec, req)

		var ml backend.ModelList
		if err := json.NewDecoder(rec.Body).Decode(&ml); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Flattened: "gpt-4o" should appear once (deduplicated) with 2 backends.
		ids := make(map[string]bool)
		for _, m := range ml.Data {
			ids[m.ID] = true
		}
		if !ids["gpt-4o"] {
			t.Error("expected gpt-4o in flattened listing")
		}
		if !ids["claude-sonnet-4"] {
			t.Error("expected claude-sonnet-4 in flattened listing")
		}
		// No backend-prefixed IDs in flattened mode.
		if ids["openrouter/gpt-4o"] || ids["copilot/gpt-4o"] {
			t.Error("flattened mode should not contain backend-prefixed IDs")
		}

		// gpt-4o should have a routing strategy. Without explicit routing config,
		// ResolveRoute uses wildcard search which returns the first matching backend.
		for _, m := range ml.Data {
			if m.ID == "gpt-4o" {
				if len(m.AvailableBackends) < 1 {
					t.Errorf("gpt-4o backends = %v, want at least 1", m.AvailableBackends)
				}
				if m.RoutingStrategy != "priority" {
					t.Errorf("gpt-4o strategy = %q, want priority", m.RoutingStrategy)
				}
			}
		}
	})

	t.Run("mode=flat returns same as default", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?mode=flat", nil)
		rec := httptest.NewRecorder()
		handler.ListModels(rec, req)

		var ml backend.ModelList
		if err := json.NewDecoder(rec.Body).Decode(&ml); err != nil {
			t.Fatalf("decode: %v", err)
		}

		ids := make(map[string]bool)
		for _, m := range ml.Data {
			ids[m.ID] = true
		}
		if !ids["gpt-4o"] {
			t.Error("expected gpt-4o in flat listing")
		}
		if ids["openrouter/gpt-4o"] {
			t.Error("flat mode should not contain backend-prefixed IDs")
		}
	})

	t.Run("mode=raw returns backend-prefixed models", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?mode=raw", nil)
		rec := httptest.NewRecorder()
		handler.ListModels(rec, req)

		var ml backend.ModelList
		if err := json.NewDecoder(rec.Body).Decode(&ml); err != nil {
			t.Fatalf("decode: %v", err)
		}

		ids := make(map[string]bool)
		for _, m := range ml.Data {
			ids[m.ID] = true
		}

		// Raw mode: each model appears once per backend, with backend prefix.
		if !ids["openrouter/gpt-4o"] {
			t.Error("expected openrouter/gpt-4o in raw listing")
		}
		if !ids["openrouter/claude-sonnet-4"] {
			t.Error("expected openrouter/claude-sonnet-4 in raw listing")
		}
		if !ids["copilot/gpt-4o"] {
			t.Error("expected copilot/gpt-4o in raw listing")
		}

		// No bare IDs in raw mode.
		if ids["gpt-4o"] || ids["claude-sonnet-4"] {
			t.Error("raw mode should not contain bare (non-prefixed) IDs")
		}

		// Each raw model has exactly 1 backend and "direct" strategy.
		for _, m := range ml.Data {
			if len(m.AvailableBackends) != 1 {
				t.Errorf("raw model %q backends = %v, want exactly 1", m.ID, m.AvailableBackends)
			}
			if m.RoutingStrategy != "direct" {
				t.Errorf("raw model %q strategy = %q, want direct", m.ID, m.RoutingStrategy)
			}
		}
	})
}
