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
	lastReq      *backend.ChatCompletionRequest
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
	reqCopy := *req
	m.lastReq = &reqCopy
	return m.chatResp, m.chatErr
}

func (m *mockBackend) ChatCompletionStream(_ context.Context, req *backend.ChatCompletionRequest) (io.ReadCloser, error) {
	m.requestCount++
	m.lastAPIKey = req.APIKeyOverride
	reqCopy := *req
	m.lastReq = &reqCopy
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

func (m *mockBackend) ClearModelCache() {}

func (m *mockBackend) ResolveModelID(canonicalID string) string { return canonicalID }

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
	handler := NewHandler(registry, collector, cfgMgr, nil)

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

func makeAnthropicRequest(t *testing.T, model string, stream bool, apiKey string) *http.Request {
	t.Helper()

	body := map[string]any{
		"model":      model,
		"max_tokens": 256,
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
		"stream": stream,
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
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
				Message:      &backend.Message{Role: "assistant", Content: json.RawMessage(`"Hello from Copilot!"`)},
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

func TestAuthMiddleware_AcceptsXAPIKey(t *testing.T) {
	cfgMgr, cleanup := newTestConfigMgr(t)
	defer cleanup()

	handler := AuthMiddleware(cfgMgr)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/messages", nil)
	req.Header.Set("x-api-key", "test-api-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestAnthropicMessages_NonStream(t *testing.T) {
	b := &mockBackend{
		name:     "zai",
		models:   []string{"glm-5.1"},
		chatResp: successResponse(),
	}
	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{"zai": b}, config.RoutingConfig{})
	defer cleanup()

	req := makeAnthropicRequest(t, "zai/glm-5.1", false, "test-api-key")
	rec := httptest.NewRecorder()
	handler.AnthropicMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if b.lastReq == nil {
		t.Fatal("expected backend request to be captured")
	}
	if b.lastReq.Model != "glm-5.1" {
		t.Fatalf("backend model = %q, want %q", b.lastReq.Model, "glm-5.1")
	}

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["type"] != "message" {
		t.Fatalf("type = %v, want message", resp["type"])
	}
	if resp["model"] != "zai/glm-5.1" {
		t.Fatalf("model = %v, want zai/glm-5.1", resp["model"])
	}
}

func TestAnthropicMessages_Stream(t *testing.T) {
	chunk1 := `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"glm-5.1","choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`
	chunk2 := `{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1,"model":"glm-5.1","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	b := &mockBackend{
		name:       "zai",
		models:     []string{"glm-5.1"},
		streamBody: sseStream(chunk1, chunk2),
	}
	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{"zai": b}, config.RoutingConfig{})
	defer cleanup()

	req := makeAnthropicRequest(t, "zai/glm-5.1", true, "test-api-key")
	rec := httptest.NewRecorder()
	handler.AnthropicMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Fatal("expected message_start event")
	}
	if !strings.Contains(body, "event: content_block_delta") {
		t.Fatal("expected content_block_delta event")
	}
	if !strings.Contains(body, `"text":"Hel"`) || !strings.Contains(body, `"text":"lo"`) {
		t.Fatal("expected translated text deltas")
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatal("expected message_stop event")
	}
}

func TestAnthropicResponseFromChat_NestedContentText(t *testing.T) {
	stop := "stop"
	resp := &backend.ChatCompletionResponse{
		Model: "glm-4.7",
		Choices: []backend.Choice{
			{
				Index: 0,
				Message: &backend.Message{
					Role:    "assistant",
					Content: json.RawMessage(`[{"type":"text","text":{"value":"Hello nested"}}]`),
				},
				FinishReason: &stop,
			},
		},
	}

	out, err := anthropicResponseFromChat(resp, "zai-anthropic/glm-4.7")
	if err != nil {
		t.Fatalf("anthropicResponseFromChat: %v", err)
	}

	var decoded struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Text != "Hello nested" {
		t.Fatalf("content = %+v, want single text block", decoded.Content)
	}
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
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	}, 0, nil)

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
	copilotUpstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	githubAPI := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      "test-copilot-token",
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

	deviceCodeHandler := oauth.NewDeviceCodeHandler(tokenStore, oauth.WithCopilotExchangerURL(githubAPI.URL))

	copilotBackend := backend.NewCopilotBackend(
		config.BackendConfig{
			Name:    "copilot",
			Type:    "copilot",
			BaseURL: copilotUpstream.URL,
		},
		deviceCodeHandler, tokenStore, nil, nil,
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

func TestRewriteStreamChunk_ExtractsChoiceLevelUsage(t *testing.T) {
	chunk := `{"id":"chatcmpl-kimi","object":"chat.completion.chunk","created":1776716734,"model":"kimi-for-coding","choices":[{"index":0,"delta":{},"finish_reason":"stop","usage":{"prompt_tokens":13,"completion_tokens":261,"total_tokens":274}}],"system_fingerprint":"fpv0_1db6139b"}`

	rewritten, usage := rewriteStreamChunk(chunk, "kimi-coding/kimi-for-coding")
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.PromptTokens != 13 {
		t.Fatalf("prompt_tokens = %d, want 13", usage.PromptTokens)
	}
	if usage.CompletionTokens != 261 {
		t.Fatalf("completion_tokens = %d, want 261", usage.CompletionTokens)
	}
	if usage.TotalTokens != 274 {
		t.Fatalf("total_tokens = %d, want 274", usage.TotalTokens)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rewritten), &m); err != nil {
		t.Fatalf("unmarshal rewritten chunk: %v", err)
	}
	if _, ok := m["usage"]; !ok {
		t.Fatal("rewritten chunk missing top-level usage")
	}
	var model string
	if err := json.Unmarshal(m["model"], &model); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}
	if model != "kimi-coding/kimi-for-coding" {
		t.Fatalf("model = %q, want %q", model, "kimi-coding/kimi-for-coding")
	}
}

// --- VAL-CODEX-006 & VAL-CODEX-007: Native Responses API passthrough ---

// mockResponsesBackend implements both Backend and ResponsesBackend for testing.
type mockResponsesBackend struct {
	mockBackend
	responsesBody         []byte // Raw response body for non-streaming
	responsesErr          error
	responsesStreamBody   string // Raw SSE stream body for streaming
	responsesStreamErr    error
	responsesRequestCount int
	lastResponsesModel    string
}

func (m *mockResponsesBackend) Responses(_ context.Context, req *backend.ResponsesRequest) (*backend.ResponsesResponse, error) {
	m.responsesRequestCount++
	m.lastResponsesModel = req.Model
	if m.responsesErr != nil {
		return nil, m.responsesErr
	}
	return &backend.ResponsesResponse{Body: m.responsesBody}, nil
}

func (m *mockResponsesBackend) ResponsesStream(_ context.Context, req *backend.ResponsesRequest) (io.ReadCloser, error) {
	m.responsesRequestCount++
	m.lastResponsesModel = req.Model
	if m.responsesStreamErr != nil {
		return nil, m.responsesStreamErr
	}
	return io.NopCloser(strings.NewReader(m.responsesStreamBody)), nil
}

// makeResponsesRequest creates an HTTP request for POST /v1/responses.
func makeResponsesRequest(t *testing.T, body map[string]any, apiKey string) *http.Request {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	return req
}

// --- VAL-CODEX-006: Non-streaming responses API passthrough ---

func TestResponses_NonStreamingPassthrough(t *testing.T) {
	// Build a Codex Responses API response body.
	codexResp := map[string]any{
		"id":     "resp_abc123",
		"object": "response",
		"status": "completed",
		"model":  "o4-mini",
		"output": []map[string]any{
			{
				"type":   "message",
				"id":     "msg_001",
				"role":   "assistant",
				"status": "completed",
				"content": []map[string]any{
					{"type": "output_text", "text": "Hello from Codex!"},
				},
			},
		},
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 5,
		},
	}
	respBody, _ := json.Marshal(codexResp)

	codexBackend := &mockResponsesBackend{
		mockBackend: mockBackend{
			name:   "codex",
			models: []string{"o4-mini", "gpt-5.2-codex"},
		},
		responsesBody: respBody,
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"codex": codexBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model":  "codex/o4-mini",
		"input":  "Say hello",
		"stream": false,
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify the response body is the raw Codex response (no translation).
	var respData map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if respData["object"] != "response" {
		t.Errorf("response object = %v, want %q", respData["object"], "response")
	}
	if respData["id"] != "resp_abc123" {
		t.Errorf("response id = %v, want %q", respData["id"], "resp_abc123")
	}
	if respData["status"] != "completed" {
		t.Errorf("response status = %v, want %q", respData["status"], "completed")
	}

	// Verify the model was stripped of prefix.
	if codexBackend.lastResponsesModel != "o4-mini" {
		t.Errorf("model sent to backend = %q, want %q", codexBackend.lastResponsesModel, "o4-mini")
	}

	// Verify backend was called exactly once.
	if codexBackend.responsesRequestCount != 1 {
		t.Errorf("responses request count = %d, want 1", codexBackend.responsesRequestCount)
	}
}

// --- VAL-CODEX-007: Streaming responses API passthrough ---

func TestResponses_StreamingPassthrough(t *testing.T) {
	// Build a Codex SSE stream with native Responses API event types.
	codexStream := "" +
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_abc\",\"status\":\"in_progress\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\" from\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\" Codex!\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_abc\",\"status\":\"completed\"}}\n\n"

	codexBackend := &mockResponsesBackend{
		mockBackend: mockBackend{
			name:   "codex",
			models: []string{"o4-mini"},
		},
		responsesStreamBody: codexStream,
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"codex": codexBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model":  "codex/o4-mini",
		"input":  "Say hello",
		"stream": true,
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// Verify the response is SSE.
	contentType := resp.Header.Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("content-type = %q, want %q", contentType, "text/event-stream")
	}

	// Read the full response body.
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}
	body := string(bodyBytes)

	// Verify Codex-specific event types are present (NOT chat completion format).
	if !strings.Contains(body, "response.created") {
		t.Error("expected 'response.created' event type in SSE stream")
	}
	if !strings.Contains(body, "response.output_text.delta") {
		t.Error("expected 'response.output_text.delta' event type in SSE stream")
	}
	if !strings.Contains(body, "response.completed") {
		t.Error("expected 'response.completed' event type in SSE stream")
	}

	// Verify NO chat completion format events are present.
	if strings.Contains(body, "chat.completion.chunk") {
		t.Error("should NOT contain 'chat.completion.chunk' — raw Codex events should be forwarded as-is")
	}

	// Verify the model was stripped of prefix.
	if codexBackend.lastResponsesModel != "o4-mini" {
		t.Errorf("model sent to backend = %q, want %q", codexBackend.lastResponsesModel, "o4-mini")
	}

	// Verify backend was called exactly once.
	if codexBackend.responsesRequestCount != 1 {
		t.Errorf("responses request count = %d, want 1", codexBackend.responsesRequestCount)
	}
}

// --- Error for backends that don't support ResponsesBackend ---

func TestResponses_UnsupportedBackend(t *testing.T) {
	// A regular mockBackend does NOT implement ResponsesBackend.
	regularBackend := &mockBackend{
		name:   "openrouter",
		models: []string{"gpt-4o"},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"openrouter": regularBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model": "openrouter/gpt-4o",
		"input": "test",
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	// Verify the error message mentions the backend doesn't support Responses API.
	var errResp map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}
	errMsg := ""
	if e, ok := errResp["error"].(map[string]any); ok {
		errMsg, _ = e["message"].(string)
	}
	if !strings.Contains(errMsg, "does not support the Responses API") {
		t.Errorf("error message = %q, want it to mention Responses API not supported", errMsg)
	}
}

// --- Responses API with missing model field ---

func TestResponses_MissingModel(t *testing.T) {
	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"input": "test",
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// --- Responses API with unknown model ---

func TestResponses_UnknownModel(t *testing.T) {
	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model": "nonexistent/model",
		"input": "test",
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// --- Responses API backend error forwarding ---

func TestResponses_BackendError(t *testing.T) {
	codexBackend := &mockResponsesBackend{
		mockBackend: mockBackend{
			name:   "codex",
			models: []string{"o4-mini"},
		},
		responsesErr: &backend.BackendError{
			StatusCode: 500,
			Body:       `{"error":{"message":"internal server error"}}`,
			Err:        fmt.Errorf("codex backend returned status 500"),
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"codex": codexBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model": "codex/o4-mini",
		"input": "test",
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// --- Responses API streaming error forwarding ---

func TestResponses_StreamingBackendError(t *testing.T) {
	codexBackend := &mockResponsesBackend{
		mockBackend: mockBackend{
			name:   "codex",
			models: []string{"o4-mini"},
		},
		responsesStreamErr: &backend.BackendError{
			StatusCode: 429,
			Body:       `{"error":{"message":"rate limit exceeded"}}`,
			Err:        fmt.Errorf("codex backend returned status 429"),
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"codex": codexBackend,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model":  "codex/o4-mini",
		"input":  "test",
		"stream": true,
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
}

// --- Method not allowed for GET on /v1/responses ---

func TestResponses_MethodNotAllowed(t *testing.T) {
	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{}, config.RoutingConfig{})
	defer cleanup()

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer test-api-key")

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}

// --- Responses API with routing config ---

func TestResponses_WithRoutingConfig(t *testing.T) {
	codexResp := map[string]any{
		"id":     "resp_routed",
		"object": "response",
		"status": "completed",
		"model":  "o4-mini",
	}
	respBody, _ := json.Marshal(codexResp)

	codexBackend := &mockResponsesBackend{
		mockBackend: mockBackend{
			name:   "codex",
			models: []string{"o4-mini"},
		},
		responsesBody: respBody,
	}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{
				Model:    "codex/o4-mini",
				Backends: []string{"codex"},
			},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"codex": codexBackend,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeResponsesRequest(t, map[string]any{
		"model": "codex/o4-mini",
		"input": "test",
	}, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.Responses(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var respData map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&respData); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if respData["id"] != "resp_routed" {
		t.Errorf("response id = %v, want %q", respData["id"], "resp_routed")
	}
}

// --- Three-backend failover chain ---

func TestThreeBackendFailover_FirstTwoFail_ThirdSucceeds(t *testing.T) {
	err5xx := &backend.BackendError{
		StatusCode: http.StatusInternalServerError,
		Body:       `{"error":{"message":"Internal Server Error"}}`,
		Err:        fmt.Errorf("backend returned 500"),
	}

	b1 := &mockBackend{name: "primary", models: []string{"gpt-4o"}, chatErr: err5xx}
	b2 := &mockBackend{name: "secondary", models: []string{"gpt-4o"}, chatErr: err5xx}
	b3 := &mockBackend{name: "tertiary", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"primary", "secondary", "tertiary"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"primary": b1, "secondary": b2, "tertiary": b3,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}
	if b1.requestCount != 1 {
		t.Errorf("primary requests = %d, want 1", b1.requestCount)
	}
	if b2.requestCount != 1 {
		t.Errorf("secondary requests = %d, want 1", b2.requestCount)
	}
	if b3.requestCount != 1 {
		t.Errorf("tertiary requests = %d, want 1", b3.requestCount)
	}
}

func TestThreeBackendFailover_AllFail_Returns502(t *testing.T) {
	err5xx := &backend.BackendError{
		StatusCode: http.StatusInternalServerError,
		Body:       `{"error":{"message":"Internal Server Error"}}`,
		Err:        fmt.Errorf("backend returned 500"),
	}

	b1 := &mockBackend{name: "a", models: []string{"gpt-4o"}, chatErr: err5xx}
	b2 := &mockBackend{name: "b", models: []string{"gpt-4o"}, chatErr: err5xx}
	b3 := &mockBackend{name: "c", models: []string{"gpt-4o"}, chatErr: err5xx}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a", "b", "c"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"a": b1, "b": b2, "c": b3,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Result().StatusCode)
	}
	// All three backends should have been tried.
	if b1.requestCount != 1 || b2.requestCount != 1 || b3.requestCount != 1 {
		t.Errorf("requests = [%d, %d, %d], want [1, 1, 1]", b1.requestCount, b2.requestCount, b3.requestCount)
	}
}

// --- 429 Rate Limit triggers fallback ---

func TestFallbackOn429_RateLimit(t *testing.T) {
	err429 := &backend.BackendError{
		StatusCode: http.StatusTooManyRequests,
		Body:       `{"error":{"message":"Rate limit exceeded"}}`,
		Err:        fmt.Errorf("rate limited"),
	}

	primary := &mockBackend{name: "primary", models: []string{"gpt-4o"}, chatErr: err429}
	fallback := &mockBackend{name: "fallback", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"primary", "fallback"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"primary": primary, "fallback": fallback,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback should succeed)", rec.Result().StatusCode)
	}
	if primary.requestCount != 1 {
		t.Errorf("primary requests = %d, want 1", primary.requestCount)
	}
	if fallback.requestCount != 1 {
		t.Errorf("fallback requests = %d, want 1", fallback.requestCount)
	}
}

// --- Network error triggers fallback ---

func TestFallbackOnNetworkError(t *testing.T) {
	primary := &mockBackend{
		name:    "primary",
		models:  []string{"gpt-4o"},
		chatErr: fmt.Errorf("connection refused"), // plain error, not BackendError
	}
	fallback := &mockBackend{name: "fallback", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"primary", "fallback"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"primary": primary, "fallback": fallback,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback should succeed)", rec.Result().StatusCode)
	}
	if fallback.requestCount != 1 {
		t.Errorf("fallback requests = %d, want 1", fallback.requestCount)
	}
}

// --- Mixed backend types: explicit routing across different types ---

func TestMixedBackends_ExplicitRouting_CorrectDispatch(t *testing.T) {
	openaiBackend := &mockBackend{name: "openai", models: []string{"gpt-4o"}, chatResp: successResponse()}
	anthropicBackend := &mockBackend{name: "anthropic", models: []string{"claude-sonnet-4"}, chatResp: successResponse()}
	copilotBackend := &mockBackend{name: "copilot", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"openai", "copilot"}},
			{Model: "claude-sonnet-4", Backends: []string{"anthropic"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"openai": openaiBackend, "anthropic": anthropicBackend, "copilot": copilotBackend,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})

	// Request gpt-4o → should go to openai (first in list).
	req1 := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req1 = req1.WithContext(ctx)
	rec1 := httptest.NewRecorder()
	handler.ChatCompletions(rec1, req1)

	if rec1.Result().StatusCode != http.StatusOK {
		t.Fatalf("gpt-4o: status = %d, want 200", rec1.Result().StatusCode)
	}
	if openaiBackend.requestCount != 1 {
		t.Errorf("openai requests = %d, want 1", openaiBackend.requestCount)
	}
	if copilotBackend.requestCount != 0 {
		t.Errorf("copilot requests = %d, want 0 (openai succeeded)", copilotBackend.requestCount)
	}

	// Request claude-sonnet-4 → should go to anthropic.
	req2 := makeChatRequest(t, "claude-sonnet-4", false, "test-api-key")
	req2 = req2.WithContext(ctx)
	rec2 := httptest.NewRecorder()
	handler.ChatCompletions(rec2, req2)

	if rec2.Result().StatusCode != http.StatusOK {
		t.Fatalf("claude: status = %d, want 200", rec2.Result().StatusCode)
	}
	if anthropicBackend.requestCount != 1 {
		t.Errorf("anthropic requests = %d, want 1", anthropicBackend.requestCount)
	}
}

// --- Streaming: three-backend failover ---

func TestStreamingThreeBackendFailover(t *testing.T) {
	streamErr := &backend.BackendError{
		StatusCode: http.StatusBadGateway,
		Body:       `{"error":"upstream error"}`,
		Err:        fmt.Errorf("stream error"),
	}

	b1 := &mockBackend{name: "a", models: []string{"gpt-4o"}, streamErr: streamErr}
	b2 := &mockBackend{name: "b", models: []string{"gpt-4o"}, streamErr: streamErr}
	b3 := &mockBackend{
		name:       "c",
		models:     []string{"gpt-4o"},
		streamBody: "data: {\"id\":\"chatcmpl-stream\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n",
	}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"a", "b", "c"}},
		},
	}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"a": b1, "b": b2, "c": b3,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", true, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}
	if b1.requestCount != 1 || b2.requestCount != 1 || b3.requestCount != 1 {
		t.Errorf("requests = [%d, %d, %d], want [1, 1, 1]", b1.requestCount, b2.requestCount, b3.requestCount)
	}
}

// --- Unknown model returns 400 ---

func TestUnknownModel_Returns400(t *testing.T) {
	b := &mockBackend{name: "openai", models: []string{"gpt-4o"}}

	handler, _, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"openai": b,
	}, config.RoutingConfig{})
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "nonexistent-model", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Result().StatusCode)
	}
}

// --- Stats record fallback flag ---

func TestStats_FallbackFlag_SetWhenSecondBackendSucceeds(t *testing.T) {
	err5xx := &backend.BackendError{
		StatusCode: http.StatusInternalServerError,
		Body:       `{"error":{"message":"error"}}`,
		Err:        fmt.Errorf("backend error"),
	}

	primary := &mockBackend{name: "primary", models: []string{"gpt-4o"}, chatErr: err5xx}
	fallback := &mockBackend{name: "fallback", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"primary", "fallback"}},
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"primary": primary, "fallback": fallback,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}

	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records")
	}
	lastRecord := records[0]
	if !lastRecord.Fallback {
		t.Error("expected Fallback=true in stats when second backend served the response")
	}
	if lastRecord.Backend != "fallback" {
		t.Errorf("backend = %q, want %q", lastRecord.Backend, "fallback")
	}
}

func TestStats_FallbackFlag_NotSetWhenFirstBackendSucceeds(t *testing.T) {
	primary := &mockBackend{name: "primary", models: []string{"gpt-4o"}, chatResp: successResponse()}
	fallback := &mockBackend{name: "fallback", models: []string{"gpt-4o"}, chatResp: successResponse()}

	routing := config.RoutingConfig{
		Models: []config.ModelRoutingConfig{
			{Model: "gpt-4o", Backends: []string{"primary", "fallback"}},
		},
	}

	handler, collector, cleanup := setupHandlerWithBackends(t, map[string]backend.Backend{
		"primary": primary, "fallback": fallback,
	}, routing)
	defer cleanup()

	ctx := context.WithValue(context.Background(), clientContextKey{}, &config.ClientConfig{Name: "test-client"})
	req := makeChatRequest(t, "gpt-4o", false, "test-api-key")
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	handler.ChatCompletions(rec, req)

	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Result().StatusCode)
	}

	records := collector.Recent(10)
	if len(records) == 0 {
		t.Fatal("expected stats records")
	}
	if records[0].Fallback {
		t.Error("expected Fallback=false when first backend succeeded")
	}
	if fallback.requestCount != 0 {
		t.Errorf("fallback should not have been called, got %d requests", fallback.requestCount)
	}
}
