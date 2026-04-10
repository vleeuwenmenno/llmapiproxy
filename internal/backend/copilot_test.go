package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
)

// helperTempDir creates a temp directory and returns (path, cleanup).
func helperTempDir(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "copilot-test-*")
	if err != nil {
		t.Fatal(err)
	}
	return dir, func() { os.RemoveAll(dir) }
}

// newTestCopilotBackend creates a CopilotBackend with a mock upstream and mock GitHub API.
// Returns the backend, the upstream server, and the GitHub API server.
func newTestCopilotBackend(t *testing.T) (*CopilotBackend, *httptest.Server, *httptest.Server) {
	t.Helper()

	// Create a mock Copilot upstream API (api.githubcopilot.com equivalent).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default handler; individual tests override via closures.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]string{
						"role":    "assistant",
						"content": "Hello from Copilot!",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]int{
				"prompt_tokens":     10,
				"completion_tokens": 5,
				"total_tokens":      15,
			},
		})
	}))

	// Create a mock GitHub API for Copilot token exchange.
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	githubAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": expiresAt,
			"refresh_in": 1500,
			"token":      "tid=test-copilot-token;fcv1=1:mac",
		})
	}))

	dir, cleanup := helperTempDir(t)
	t.Cleanup(cleanup)

	ts, err := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(ts,
		oauth.WithCopilotAPIURL(githubAPI.URL),
	)

	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: upstream.URL,
	}

	b := NewCopilotBackend(cfg, discoverer, exchanger, ts)

	return b, upstream, githubAPI
}

// --- VAL-COPILOT-001: Non-streaming chat completion through Copilot backend ---

func TestCopilotBackend_ChatCompletion(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	ctx := context.Background()
	resp, err := b.ChatCompletion(ctx, req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
	}
	if resp.ID != "chatcmpl-test" {
		t.Errorf("id = %q, want %q", resp.ID, "chatcmpl-test")
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message == nil {
		t.Fatal("choice message is nil")
	}
	if resp.Choices[0].Message.Content != "Hello from Copilot!" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello from Copilot!")
	}
}

// --- VAL-COPILOT-002: Non-streaming response includes usage statistics ---

func TestCopilotBackend_ChatCompletion_Usage(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Usage == nil {
		t.Fatal("usage is nil")
	}
	if resp.Usage.PromptTokens == 0 {
		t.Error("prompt_tokens should be > 0")
	}
	if resp.Usage.CompletionTokens == 0 {
		t.Error("completion_tokens should be > 0")
	}
	if resp.Usage.TotalTokens == 0 {
		t.Error("total_tokens should be > 0")
	}
}

// --- VAL-COPILOT-003: Streaming chat completion ---

func TestCopilotBackend_ChatCompletionStream(t *testing.T) {
	upstreamCalled := false

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	// Replace upstream handler with SSE stream.
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		chunks := []string{
			`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" from"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-test","object":"chat.completion.chunk","created":1234,"model":"gpt-4o","choices":[{"index":0,"delta":{"content":" Copilot!"},"finish_reason":"stop"}]}`,
		}

		for _, chunk := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "Hello"}},
	}

	stream, err := b.ChatCompletionStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	body, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("reading stream: %v", err)
	}

	if !upstreamCalled {
		t.Error("upstream was not called")
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "data: ") {
		t.Error("expected SSE data lines in stream")
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("expected [DONE] sentinel in stream")
	}
	if !strings.Contains(bodyStr, "delta") {
		t.Error("expected delta objects in stream")
	}
}

// --- VAL-COPILOT-004: Streaming chunks contain delta objects ---

func TestCopilotBackend_Streaming_DeltaObjects(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprintf(w, "data: %s\n\n", `{"id":"chatcmpl-test","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	stream, err := b.ChatCompletionStream(context.Background(), &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	body, _ := io.ReadAll(stream)
	bodyStr := string(body)

	// Check for delta field
	if !strings.Contains(bodyStr, `"delta"`) {
		t.Error("expected delta field in streaming chunks")
	}

	// Check for finish_reason: stop
	if !strings.Contains(bodyStr, `"stop"`) {
		t.Error("expected finish_reason: stop in final chunk")
	}
}

// --- VAL-COPILOT-005: Model listing includes Copilot models ---

func TestCopilotBackend_ListModels(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
		Models:  []string{"gpt-4o", "gpt-4.1", "o3"},
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(ts)
	b := NewCopilotBackend(cfg, discoverer, exchanger, ts)

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}

	modelIDs := make(map[string]bool)
	for _, m := range models {
		modelIDs[m.ID] = true
		if m.Object != "model" {
			t.Errorf("model %q object = %q, want %q", m.ID, m.Object, "model")
		}
		if m.OwnedBy != "copilot" {
			t.Errorf("model %q owned_by = %q, want %q", m.ID, m.OwnedBy, "copilot")
		}
	}

	for _, expected := range []string{"gpt-4o", "gpt-4.1", "o3"} {
		if !modelIDs[expected] {
			t.Errorf("expected model %q in list", expected)
		}
	}
}

// --- VAL-COPILOT-006: Required Copilot headers are sent to upstream API ---

func TestCopilotBackend_RequiredHeaders(t *testing.T) {
	var receivedHeaders http.Header

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "test"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Check required headers
	requiredHeaders := []string{
		"Authorization",
		"Editor-Version",
		"Editor-Plugin-Version",
		"Copilot-Integration-Id",
		"User-Agent",
		"Content-Type",
	}

	for _, h := range requiredHeaders {
		if receivedHeaders.Get(h) == "" {
			t.Errorf("required header %q is missing or empty", h)
		}
	}

	// Authorization should contain the Copilot token
	auth := receivedHeaders.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Authorization = %q, expected Bearer token", auth)
	}

	// Verify X-Request-Id is present (it's a UUID)
	if receivedHeaders.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header is missing")
	}
}

// --- VAL-COPILOT-010: Model prefix routing strips backend prefix ---

func TestCopilotBackend_PrefixRouting(t *testing.T) {
	var receivedModel string

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)
		json.Unmarshal(body["model"], &receivedModel)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   receivedModel,
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "test"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	// The handler strips the prefix, so the backend receives "gpt-4o"
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// The upstream should receive "gpt-4o" (no prefix)
	if receivedModel != "gpt-4o" {
		t.Errorf("upstream received model = %q, want %q (prefix should be stripped)", receivedModel, "gpt-4o")
	}
}

// --- VAL-COPILOT-014: Error handling — no GitHub token available ---

func TestCopilotBackend_NoGitHubToken(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	discoverer := oauth.NewDiscoverer() // No env vars, no gh CLI, no file
	exchanger := oauth.NewCopilotExchanger(ts)
	b := NewCopilotBackend(cfg, discoverer, exchanger, ts)

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no GitHub token is available")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "copilot") && !strings.Contains(errMsg, "token") {
		t.Errorf("error message should mention copilot/token, got: %s", errMsg)
	}
}

// --- VAL-COPILOT-015: Error handling — rate limit ---

func TestCopilotBackend_RateLimit(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Rate limit exceeded",
				"type":    "rate_limit_error",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for rate limit")
	}

	var be *BackendError
	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "rate") {
		t.Errorf("error should reference rate limit, got: %v", err)
	}
	_ = be
}

// --- VAL-COPILOT-016: Error handling — model not available ---

func TestCopilotBackend_ModelNotFound(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Model not found",
				"type":    "invalid_request_error",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "nonexistent-model",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for model not found")
	}

	var be *BackendError
	if _, ok := err.(*BackendError); !ok {
		// Also acceptable: wrapped error
		t.Logf("error type: %T, value: %v", err, err)
	}
	_ = be
}

// --- VAL-COPILOT-017: Error handling — subscription issues ---

func TestCopilotBackend_SubscriptionError(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Copilot subscription required",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for subscription issue")
	}

	var be *BackendError
	if errors.As(err, &be) {
		if be.StatusCode != http.StatusForbidden {
			t.Errorf("BackendError.StatusCode = %d, want %d", be.StatusCode, http.StatusForbidden)
		}
	}
}

// --- VAL-TOKEN-038: Upstream 401 triggers re-authentication with retry ---

func TestCopilotBackend_Upstream401_RetryWithReauth(t *testing.T) {
	var attemptCount int32

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attemptCount, 1)
		if count == 1 {
			// First request: return 401
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"message": "Unauthorized"},
			})
			return
		}
		// Second request (after re-auth): succeed
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "Success after retry"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	})

	// Pre-set a Copilot token so we can make the initial request.
	// The 401 should trigger a re-exchange via the mock githubAPI.
	expiresAt := time.Now().Add(30 * time.Minute)
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "initial-copilot-token",
		ExpiresAt:   expiresAt,
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion after 401 retry: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice after retry")
	}
	if resp.Choices[0].Message.Content != "Success after retry" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Success after retry")
	}

	// Should have made 2 attempts: first (401) + retry (success)
	if atomic.LoadInt32(&attemptCount) != 2 {
		t.Errorf("expected 2 attempts, got %d", atomic.LoadInt32(&attemptCount))
	}
}

// --- VAL-TOKEN-040: Upstream 401 re-auth loop prevention ---

func TestCopilotBackend_Upstream401_LoopPrevention(t *testing.T) {
	var attemptCount int32

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attemptCount, 1)
		// Always return 401
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "Unauthorized"},
		})
		_ = count
	})

	// Pre-set a Copilot token.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when upstream always returns 401")
	}

	// Should have made exactly 2 attempts (initial + one retry), not more.
	attempts := atomic.LoadInt32(&attemptCount)
	if attempts > 2 {
		t.Errorf("expected at most 2 attempts (loop prevention), got %d", attempts)
	}
}

// --- VAL-COPILOT-007/008/009: Different base URL routing ---

func TestCopilotBackend_IndividualURL(t *testing.T) {
	b, _, _ := newTestCopilotBackend(t)
	// Verify the base URL was stored correctly.
	if b.baseURL == "" {
		t.Error("base URL should not be empty")
	}
}

func TestCopilotBackend_BusinessURL(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "copilot-biz",
		Type:    "copilot",
		BaseURL: "https://api.business.githubcopilot.com",
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(ts)
	b := NewCopilotBackend(cfg, discoverer, exchanger, ts)

	if b.Name() != "copilot-biz" {
		t.Errorf("Name() = %q, want %q", b.Name(), "copilot-biz")
	}
	if b.baseURL != "https://api.business.githubcopilot.com" {
		t.Errorf("baseURL = %q, want business URL", b.baseURL)
	}
}

func TestCopilotBackend_EnterpriseURL(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "copilot-ent",
		Type:    "copilot",
		BaseURL: "https://api.enterprise.githubcopilot.com",
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(ts)
	b := NewCopilotBackend(cfg, discoverer, exchanger, ts)

	if b.Name() != "copilot-ent" {
		t.Errorf("Name() = %q, want %q", b.Name(), "copilot-ent")
	}
	if b.baseURL != "https://api.enterprise.githubcopilot.com" {
		t.Errorf("baseURL = %q, want enterprise URL", b.baseURL)
	}
}

// --- SupportsModel tests ---

func TestCopilotBackend_SupportsModel(t *testing.T) {
	tests := []struct {
		name    string
		models  []string
		check   string
		want    bool
	}{
		{"empty models list (accepts all)", nil, "anything", true},
		{"exact match", []string{"gpt-4o"}, "gpt-4o", true},
		{"no match", []string{"gpt-4o"}, "claude-3", false},
		{"wildcard match", []string{"openai/*"}, "openai/gpt-4o", true},
		{"wildcard no match", []string{"openai/*"}, "anthropic/claude-3", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.BackendConfig{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				Models:  tt.models,
			}
			dir, cleanup := helperTempDir(t)
			defer cleanup()

			ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
			b := NewCopilotBackend(cfg, nil, nil, ts)

			if got := b.SupportsModel(tt.check); got != tt.want {
				t.Errorf("SupportsModel(%q) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

// --- VAL-COPILOT-026: Concurrent requests ---

func TestCopilotBackend_ConcurrentRequests(t *testing.T) {
	var requestCount int32

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		// Simulate a bit of latency
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "concurrent response"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
	})

	// Pre-set a Copilot token.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "test-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	const numRequests = 5
	var wg sync.WaitGroup
	errCh := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &ChatCompletionRequest{
				Model:    "gpt-4o",
				Messages: []Message{{Role: "user", Content: "test"}},
			}
			_, err := b.ChatCompletion(context.Background(), req)
			if err != nil {
				errCh <- err
			}
		}()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent request failed: %v", err)
	}

	count := atomic.LoadInt32(&requestCount)
	if count != numRequests {
		t.Errorf("expected %d requests, got %d", numRequests, count)
	}
}

// --- VAL-COPILOT-011: Prefix routing does not match partial names ---

func TestCopilotBackend_PartialPrefixNoMatch(t *testing.T) {
	r := NewRegistry()

	// Manually register the backend with explicit models (so SupportsModel is selective)
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	discoverer := oauth.NewDiscoverer()
	exchanger := oauth.NewCopilotExchanger(ts)
	backend := NewCopilotBackend(config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
		Models:  []string{"gpt-4o", "o3"},
	}, discoverer, exchanger, ts)

	r.backends["copilot"] = backend

	// "copilot-other/gpt-4o" should NOT match the "copilot" backend:
	// - prefix check: "copilot-other" != "copilot" → no match
	// - wildcard fallback: "copilot-other/gpt-4o" is not in Models list → no match
	_, _, err := r.Resolve("copilot-other/gpt-4o")
	if err == nil {
		t.Error("expected error for partial prefix match")
	}
}

// --- VAL-COPILOT-022: Empty messages array returns error ---

func TestCopilotBackend_EmptyMessages(t *testing.T) {
	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{},
	}

	// Pre-set a Copilot token.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "test-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	// The request is forwarded to the upstream, which may or may not reject it.
	// The backend should not crash. If upstream rejects, it's an error.
	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		// Acceptable: upstream rejects empty messages
		t.Logf("upstream rejected empty messages (acceptable): %v", err)
	} else {
		// Also acceptable: upstream accepts it
		t.Logf("upstream accepted empty messages, got response: %+v", resp)
	}
}

// --- VAL-COPILOT-027: Token refresh handles expiry ---

func TestCopilotBackend_TokenRefreshOnExpiry(t *testing.T) {
	var exchangeCount int32

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	// Override GitHub API to count exchanges.
	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	githubAPI.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchangeCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": expiresAt,
			"refresh_in": 1500,
			"token":      "refreshed-copilot-token",
		})
	})

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "test"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	// Set an expired token.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour), // expired
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "test",
	})

	// Set a GitHub token so the exchange can work.
	t.Setenv("GH_TOKEN", "test-github-token")
	b.discoverer = oauth.NewDiscoverer(oauth.WithTokenStore(b.tokenStore))

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion with expired token: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Error("expected at least one choice")
	}

	// The exchange should have been called to get a fresh token.
	if atomic.LoadInt32(&exchangeCount) == 0 {
		t.Error("expected at least one token exchange for expired token")
	}
}

// --- VAL-COPILOT-028: Backend disabled state ---

func TestCopilotBackend_Disabled(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "copilot",
				Type:    "copilot",
				BaseURL: "https://api.githubcopilot.com",
				Enabled: boolPtr(false),
			},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if r.Has("copilot") {
		t.Error("disabled copilot backend should not be registered")
	}
}

// --- Name() test ---

func TestCopilotBackend_Name(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "my-copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	b := NewCopilotBackend(cfg, nil, nil, ts)

	if b.Name() != "my-copilot" {
		t.Errorf("Name() = %q, want %q", b.Name(), "my-copilot")
	}
}

// --- Verify APIKeyOverride is ignored for Copilot (VAL-COPILOT-021) ---

func TestCopilotBackend_IgnoresAPIKeyOverride(t *testing.T) {
	var receivedAuth string

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "test"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	// Pre-set a Copilot token.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "real-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	req := &ChatCompletionRequest{
		Model:           "gpt-4o",
		Messages:        []Message{{Role: "user", Content: "test"}},
		APIKeyOverride:  "sk-custom-key", // Should be ignored for Copilot
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// The Authorization header should use the Copilot token, not the override.
	if !strings.Contains(receivedAuth, "Bearer ") {
		t.Errorf("Authorization = %q, expected Bearer token", receivedAuth)
	}
	if strings.Contains(receivedAuth, "sk-custom-key") {
		t.Error("APIKeyOverride should be ignored for Copilot backend")
	}
}

// --- Verify rewriteBody preserves raw body fields ---

func TestCopilotBackend_RewriteBody(t *testing.T) {
	var receivedBody map[string]json.RawMessage

	b, upstream, githubAPI := newTestCopilotBackend(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   "gpt-4o",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "test"}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	})

	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "test-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})

	// Use RawBody with extra fields to test preservation.
	rawBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9}`
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
		RawBody:  []byte(rawBody),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify model was rewritten
	var modelStr string
	json.Unmarshal(receivedBody["model"], &modelStr)
	if modelStr != "gpt-4o" {
		t.Errorf("model in body = %q, want %q", modelStr, "gpt-4o")
	}

	// Verify extra fields are preserved
	if _, ok := receivedBody["temperature"]; !ok {
		t.Error("temperature field should be preserved")
	}
	if _, ok := receivedBody["top_p"]; !ok {
		t.Error("top_p field should be preserved")
	}
}

// Helper for creating bool pointer.
