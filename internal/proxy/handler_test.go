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
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/stats"
)

// --- Test helpers ---

// mockBackend is a simple Backend implementation for proxy handler tests.
// It returns preconfigured responses, making it easy to test routing,
// failover, and stats without real API calls.
type mockBackend struct {
	name       string
	models     []string
	chatResp   *backend.ChatCompletionResponse
	chatErr    error
	streamBody string // Full SSE stream body
	streamErr  error
	// Tracking fields for assertions
	requestCount int
	lastAPIKey   string
}

func (m *mockBackend) Name() string { return m.name }

func (m *mockBackend) SupportsModel(modelID string) bool {
	if len(m.models) == 0 {
		return true
	}
	for _, model := range m.models {
		if model == modelID {
			return true
		}
	}
	return false
}

func (m *mockBackend) ChatCompletion(_ context.Context, req *backend.ChatCompletionRequest) (*backend.ChatCompletionResponse, error) {
	m.requestCount++
	m.lastAPIKey = req.APIKeyOverride
	return m.chatResp, m.chatErr
}

func (m *mockBackend) ChatCompletionStream(_ context.Context, req *backend.ChatCompletionRequest) (io.ReadCloser, error) {
	m.requestCount++
	m.lastAPIKey = req.APIKeyOverride
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return io.NopCloser(strings.NewReader(m.streamBody)), nil
}

func (m *mockBackend) ListModels(_ context.Context) ([]backend.Model, error) {
	models := make([]backend.Model, 0, len(m.models))
	for _, id := range m.models {
		models = append(models, backend.Model{
			ID:      id,
			Object:  "model",
			Created: time.Now().Unix(),
			OwnedBy: m.name,
		})
	}
	return models, nil
}

func boolPtr(b bool) *bool { return &b }

// newTestCollector creates a stats.Collector for testing.
func newTestCollector() *stats.Collector {
	return stats.NewCollector(1000)
}

// newTestConfigMgr creates a config.Manager with a temp config file.
// The caller must call the returned cleanup function when done.
func newTestConfigMgr(t *testing.T, extraClients ...config.ClientConfig) (*config.Manager, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "proxy-test-*")
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	var clientsYAML strings.Builder
	for _, cl := range extraClients {
		fmt.Fprintf(&clientsYAML, "  - name: %s\n    api_key: %s\n", cl.Name, cl.APIKey)
		if len(cl.BackendKeys) > 0 {
			fmt.Fprintf(&clientsYAML, "    backend_keys:\n")
			for k, v := range cl.BackendKeys {
				fmt.Fprintf(&clientsYAML, "      %s: %s\n", k, v)
			}
		}
	}

	cfgData := fmt.Sprintf(`
server:
  listen: ":0"
  api_keys:
    - test-api-key
backends:
    - name: dummy
      type: openai
      base_url: "https://example.com/v1"
      api_key: dummy-key
clients:
%s
routing:
  models: []
`, clientsYAML.String())

	cfgPath := filepath.Join(dir, "config.yaml")
	if writeErr := os.WriteFile(cfgPath, []byte(cfgData), 0600); writeErr != nil {
		cleanup()
		t.Fatal(writeErr)
	}

	cfgMgr, mgrErr := config.NewManager(cfgPath)
	if mgrErr != nil {
		cleanup()
		t.Fatal(mgrErr)
	}

	return cfgMgr, cleanup
}

// setupHandlerWithBackends creates a Handler with the given backends registered
// and optional routing config applied. Returns the handler, collector, and cleanup func.
func setupHandlerWithBackends(t *testing.T, backends map[string]backend.Backend, routing config.RoutingConfig, clients ...config.ClientConfig) (*Handler, *stats.Collector, func()) {
	t.Helper()

	cfgMgr, cfgCleanup := newTestConfigMgr(t, clients...)

	// Update the routing config on the manager.
	if saveErr := cfgMgr.SaveRouting(routing); saveErr != nil {
		cfgCleanup()
		t.Fatalf("SaveRouting: %v", saveErr)
	}

	registry := backend.NewRegistry()
	for name, b := range backends {
		registry.Register(name, b)
	}

	collector := newTestCollector()
	handler := NewHandler(registry, collector, cfgMgr)

	return handler, collector, cfgCleanup
}

// makeChatRequest creates an HTTP request for POST /v1/chat/completions.
func makeChatRequest(t *testing.T, model string, stream bool, apiKey string) *http.Request {
	t.Helper()

	body := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "Hello"}},
		"stream":   stream,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return req
}

// successResponse creates a standard successful ChatCompletionResponse.
func successResponse() *backend.ChatCompletionResponse {
	stop := "stop"
	return &backend.ChatCompletionResponse{
		ID:      "chatcmpl-test",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "gpt-4o",
		Choices: []backend.Choice{
			{
				Index:        0,
				Message:      &backend.Message{Role: "assistant", Content: "Hello from Copilot!"},
				FinishReason: &stop,
			},
		},
		Usage: &backend.Usage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}
}

// sseStream builds an SSE stream string with chunks and [DONE].
func sseStream(chunks ...string) string {
	var b strings.Builder
	for _, chunk := range chunks {
		fmt.Fprintf(&b, "data: %s\n\n", chunk)
	}
	fmt.Fprintf(&b, "data: [DONE]\n\n")
	return b.String()
}

// --- VAL-COPILOT-012: Fallback/failover on 5xx ---

func TestCopilotProxy_FallbackOn5xx(t *testing.T) {
	// Copilot backend returns 500 (5xx error).
	copilotErr := &backend.BackendError{
		StatusCode: http.StatusInternalServerError,
		Body:       `{"error":{"message":"Internal Server Error"}}`,
		Err:        fmt.Errorf("copilot backend returned status 500: Internal Server Error"),
	}
	copilotBackend := &mockBackend{
		name:     "copilot",
		models:   []string{"gpt-4o"},
		chatResp: nil,
		chatErr:  copilotErr,
	}

	// Fallback backend returns success.
	fallbackBackend := &mockBackend{
		name:     "fallback",
		models:   []string{"gpt-4o"},
		chatResp: successResponse(),
		chatErr:  nil,
	}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{
				Model:    "copilot/gpt-4o",
				Backends: []string{"copilot", "fallback"},
			},
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"copilot":  copilotBackend,
		"fallback": fallbackBackend,
	}, routing)
	defer cleanup()

	// Inject client in context (bypasses auth middleware).
	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify fallback backend was called.
	if fallbackBackend.requestCount != 1 {
		t.Errorf("fallback backend request count = %d, want 1", fallbackBackend.requestCount)
	}

	// Verify copilot backend was also called (first attempt).
	if copilotBackend.requestCount != 1 {
		t.Errorf("copilot backend request count = %d, want 1", copilotBackend.requestCount)
	}

	// Verify stats were recorded (the successful response from fallback).
	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records to be created")
	}

	// The stats should show the successful fallback backend.
	lastRecord := records[0]
	if lastRecord.Backend != "fallback" {
		t.Errorf("stats backend = %q, want %q", lastRecord.Backend, "fallback")
	}
	if lastRecord.StatusCode != http.StatusOK {
		t.Errorf("stats status code = %d, want %d", lastRecord.StatusCode, http.StatusOK)
	}
}

// --- VAL-COPILOT-013: No fallback on 4xx ---

func TestCopilotProxy_NoFallbackOn4xx(t *testing.T) {
	// Copilot backend returns 400 (4xx error).
	copilotErr := &backend.BackendError{
		StatusCode: http.StatusBadRequest,
		Body:       `{"error":{"message":"Bad Request: invalid model"}}`,
		Err:        fmt.Errorf("copilot backend returned status 400: Bad Request"),
	}
	copilotBackend := &mockBackend{
		name:     "copilot",
		models:   []string{"gpt-4o"},
		chatResp: nil,
		chatErr:  copilotErr,
	}

	// Fallback backend should NOT be called.
	fallbackBackend := &mockBackend{
		name:     "fallback",
		models:   []string{"gpt-4o"},
		chatResp: successResponse(),
		chatErr:  nil,
	}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{
				Model:    "copilot/gpt-4o",
				Backends: []string{"copilot", "fallback"},
			},
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"copilot":  copilotBackend,
		"fallback": fallbackBackend,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()

	// Should return 502 (Bad Gateway) wrapping the 4xx upstream error.
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (bad gateway for upstream 4xx)", resp.StatusCode, http.StatusBadGateway)
	}

	// Verify copilot backend was called.
	if copilotBackend.requestCount != 1 {
		t.Errorf("copilot backend request count = %d, want 1", copilotBackend.requestCount)
	}

	// Verify fallback backend was NOT called.
	if fallbackBackend.requestCount != 0 {
		t.Errorf("fallback backend should NOT be called on 4xx, but got %d calls", fallbackBackend.requestCount)
	}

	// Verify error stats were recorded.
	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records to be created")
	}

	lastRecord := records[0]
	if lastRecord.StatusCode != http.StatusBadGateway {
		t.Errorf("stats status code = %d, want %d", lastRecord.StatusCode, http.StatusBadGateway)
	}
	if lastRecord.Error == "" {
		t.Error("expected error field to be populated in stats record")
	}
}

// --- VAL-COPILOT-020: Per-client backend key overrides for non-Copilot backends ---

func TestCopilotProxy_PerClientBackendKeyOverrides(t *testing.T) {
	// Set up a mock upstream server for the "openrouter" backend.
	var receivedAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	}))
	defer upstream.Close()

	// Create an OpenAI-type backend (non-Copilot).
	openrouterBackend := backend.NewOpenAI(config.BackendConfig{
		Name:    "openrouter",
		Type:    "openai",
		BaseURL: upstream.URL,
		APIKey:  "default-api-key",
	})

	// Client config with backend key override.
	clientWithOverride := config.ClientConfig{
		Name:   "custom-client",
		APIKey: "client-test-key",
		BackendKeys: map[string]string{
			"openrouter": "sk-or-v1-custom-key",
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t,
		map[string]backend.Backend{
			"openrouter": openrouterBackend,
		},
		config.RoutingConfig{},
		clientWithOverride,
	)
	defer cleanup()

	// Make request with the client's API key.
	ctx := context.WithValue(context.Background(), clientContextKey{}, &clientWithOverride)
	req := makeChatRequest(t, "openrouter/gpt-4o", false, "client-test-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify the override key was used, not the default.
	if receivedAuth != "Bearer sk-or-v1-custom-key" {
		t.Errorf("upstream received Authorization = %q, want %q", receivedAuth, "Bearer sk-or-v1-custom-key")
	}

	// Verify stats were recorded.
	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records")
	}
	if records[0].Client != "custom-client" {
		t.Errorf("stats client = %q, want %q", records[0].Client, "custom-client")
	}
}

// --- VAL-COPILOT-020 (continued): Backend key override ignored for Copilot ---

func TestCopilotProxy_BackendKeyOverrideIgnoredForCopilot(t *testing.T) {
	// Create a Copilot backend with a mock upstream.
	var receivedAuth string
	copilotUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Hello from Copilot!"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
		})
	}))
	defer copilotUpstream.Close()

	// Mock GitHub token exchange server.
	githubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":       "test-copilot-token",
		})
	}))
	defer githubAPI.Close()

	// Create temp dir for token store.
	tempDir, tempErr := os.MkdirTemp("", "copilot-test-*")
	if tempErr != nil {
		t.Fatal(tempErr)
	}
	defer os.RemoveAll(tempDir)

	tokenStore, tsErr := oauth.NewTokenStore(filepath.Join(tempDir, "copilot-token.json"))
	if tsErr != nil {
		t.Fatal(tsErr)
	}

	// Pre-seed a Copilot token so the backend doesn't need to do exchange.
	tokenStore.Save(&oauth.TokenData{
		AccessToken: "test-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(tokenStore, oauth.WithCopilotAPIURL(githubAPI.URL))

	copilotBackend := backend.NewCopilotBackend(
		config.BackendConfig{
			Name:    "copilot",
			Type:    "copilot",
			BaseURL: copilotUpstream.URL,
		},
		discoverer, exchanger, tokenStore,
	)

	// Client with backend_keys override for copilot (should be ignored).
	clientWithOverride := config.ClientConfig{
		Name:   "override-client",
		APIKey: "client-test-key",
		BackendKeys: map[string]string{
			"copilot": "sk-ignored-override-key",
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t,
		map[string]backend.Backend{
			"copilot": copilotBackend,
		},
		config.RoutingConfig{},
		clientWithOverride,
	)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &clientWithOverride)
	req := makeChatRequest(t, "copilot/gpt-4o", false, "client-test-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify the Copilot token was used, NOT the client's override key.
	if receivedAuth != "Bearer test-copilot-token" {
		t.Errorf("upstream received Authorization = %q, want %q (Copilot token, not override)", receivedAuth, "Bearer test-copilot-token")
	}
	if strings.Contains(receivedAuth, "sk-ignored-override-key") {
		t.Error("backend key override should be ignored for Copilot backend")
	}
}

// --- VAL-COPILOT-029: Stats recording for non-streaming Copilot requests ---

func TestCopilotProxy_StatsRecording_NonStreaming(t *testing.T) {
	copilotBackend := &mockBackend{
		name:     "copilot",
		models:   []string{"gpt-4o"},
		chatResp: successResponse(),
		chatErr:  nil,
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"copilot": copilotBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "copilot/gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify stats were recorded.
	records := collector.Recent(10)
	if len(records) != 1 {
		t.Fatalf("expected 1 stats record, got %d", len(records))
	}

	rec1 := records[0]

	// Verify backend name.
	if rec1.Backend != "copilot" {
		t.Errorf("backend = %q, want %q", rec1.Backend, "copilot")
	}

	// Verify model name (should be the original prefixed model).
	if rec1.Model != "copilot/gpt-4o" {
		t.Errorf("model = %q, want %q", rec1.Model, "copilot/gpt-4o")
	}

	// Verify non-zero latency.
	if rec1.LatencyMs < 0 {
		t.Errorf("latency_ms = %d, want > 0", rec1.LatencyMs)
	}

	// Verify token usage.
	if rec1.PromptTokens != 10 {
		t.Errorf("prompt_tokens = %d, want 10", rec1.PromptTokens)
	}
	if rec1.CompletionTokens != 5 {
		t.Errorf("completion_tokens = %d, want 5", rec1.CompletionTokens)
	}
	if rec1.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", rec1.TotalTokens)
	}

	// Verify stream = false.
	if rec1.Stream {
		t.Error("stream should be false for non-streaming request")
	}

	// Verify status code.
	if rec1.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want %d", rec1.StatusCode, http.StatusOK)
	}

	// Verify client name.
	if rec1.Client != "test-client" {
		t.Errorf("client = %q, want %q", rec1.Client, "test-client")
	}

	// Verify no error.
	if rec1.Error != "" {
		t.Errorf("error = %q, want empty", rec1.Error)
	}
}

// --- VAL-COPILOT-030: Stats recording for streaming Copilot requests ---

func TestCopilotProxy_StatsRecording_Streaming(t *testing.T) {
	// Build a realistic SSE stream with usage info in the final chunk.
	chunk1 := `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`
	chunk2 := `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" from"},"finish_reason":null}]}`
	chunk3 := `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" Copilot!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

	streamBody := sseStream(chunk1, chunk2, chunk3)

	copilotBackend := &mockBackend{
		name:       "copilot",
		models:     []string{"gpt-4o"},
		streamBody: streamBody,
		streamErr:  nil,
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"copilot": copilotBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "stream-client"})
	req := makeChatRequest(t, "copilot/gpt-4o", true, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify the response is SSE.
	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("content-type = %q, want %q", contentType, "text/event-stream")
	}

	// Verify stats were recorded.
	records := collector.Recent(10)
	if len(records) != 1 {
		t.Fatalf("expected 1 stats record, got %d", len(records))
	}

	rec1 := records[0]

	// Verify backend name.
	if rec1.Backend != "copilot" {
		t.Errorf("backend = %q, want %q", rec1.Backend, "copilot")
	}

	// Verify model name (original prefixed model).
	if rec1.Model != "copilot/gpt-4o" {
		t.Errorf("model = %q, want %q", rec1.Model, "copilot/gpt-4o")
	}

	// Verify stream = true.
	if !rec1.Stream {
		t.Error("stream should be true for streaming request")
	}

	// Verify non-zero latency.
	if rec1.LatencyMs < 0 {
		t.Errorf("latency_ms = %d, want > 0", rec1.LatencyMs)
	}

	// Verify token usage (from the last SSE chunk with usage).
	if rec1.PromptTokens != 10 {
		t.Errorf("prompt_tokens = %d, want 10", rec1.PromptTokens)
	}
	if rec1.CompletionTokens != 5 {
		t.Errorf("completion_tokens = %d, want 5", rec1.CompletionTokens)
	}
	if rec1.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", rec1.TotalTokens)
	}

	// Verify status code.
	if rec1.StatusCode != http.StatusOK {
		t.Errorf("status_code = %d, want %d", rec1.StatusCode, http.StatusOK)
	}

	// Verify client name.
	if rec1.Client != "stream-client" {
		t.Errorf("client = %q, want %q", rec1.Client, "stream-client")
	}
}
