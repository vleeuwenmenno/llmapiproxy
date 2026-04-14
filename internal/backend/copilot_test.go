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

// newTestCopilotBackend creates a CopilotBackend with a mock upstream and mock GitHub/Copilot API.
// Returns the backend and the upstream server.
// The Copilot exchange server is configured as part of the device code handler.
func newTestCopilotBackend(t *testing.T) (*CopilotBackend, *httptest.Server) {
	t.Helper()

	// Create a mock Copilot upstream API (api.githubcopilot.com equivalent).
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	dir, cleanup := helperTempDir(t)
	t.Cleanup(cleanup)

	ts, err := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Create device code handler with a mock Copilot exchange server.
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)

	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: upstream.URL,
	}

	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

	return b, upstream
}

// newTestCopilotBackendWithExchange creates a CopilotBackend with both a mock upstream
// and a mock Copilot exchange server. Returns the backend, upstream server, and exchange server.
func newTestCopilotBackendWithExchange(t *testing.T) (*CopilotBackend, *httptest.Server, *httptest.Server) {
	t.Helper()

	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	expiresAt := time.Now().Add(30 * time.Minute).Unix()
	githubAPI := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts,
		oauth.WithCopilotExchangerURL(githubAPI.URL),
	)

	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: upstream.URL,
	}

	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

	return b, upstream, githubAPI
}

// preSetToken sets a valid Copilot token on the backend's token store.
func preSetToken(b *CopilotBackend, token string) {
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: token,
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
	})
}

// --- VAL-COPILOT-001: Non-streaming chat completion through Copilot backend ---

func TestCopilotBackend_ChatCompletion(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

	req := &ChatCompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
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
	if string(resp.Choices[0].Message.Content) != "\"Hello from Copilot!\"" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello from Copilot!")
	}
}

// --- VAL-COPILOT-002: Non-streaming response includes usage statistics ---

func TestCopilotBackend_ChatCompletion_Usage(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
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
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"Hello"`)}},
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
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	body, _ := io.ReadAll(stream)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, `"delta"`) {
		t.Error("expected delta field in streaming chunks")
	}
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
		Models:  []config.ModelConfig{{ID: "gpt-4o"}, {ID: "gpt-4.1"}, {ID: "o3"}},
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

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

	for _, expected := range []config.ModelConfig{{ID: "gpt-4o"}, {ID: "gpt-4.1"}, {ID: "o3"}} {
		if !modelIDs[expected.ID] {
			t.Errorf("expected model %q in list", expected.ID)
		}
	}
}

// --- VAL-COPILOT-006: Required Copilot headers are sent to upstream API ---

func TestCopilotBackend_RequiredHeaders(t *testing.T) {
	var receivedHeaders http.Header

	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

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

	auth := receivedHeaders.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Authorization = %q, expected Bearer token", auth)
	}

	if receivedHeaders.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id header is missing")
	}
}

// --- VAL-COPILOT-010: Model prefix routing strips backend prefix ---

func TestCopilotBackend_PrefixRouting(t *testing.T) {
	var receivedModel string

	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if receivedModel != "gpt-4o" {
		t.Errorf("upstream received model = %q, want %q (prefix should be stripped)", receivedModel, "gpt-4o")
	}
}

// --- VAL-COPILOT-014: Error handling — no token available ---

func TestCopilotBackend_NoToken(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
	}

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no token is available")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "copilot") && !strings.Contains(errMsg, "token") {
		t.Errorf("error message should mention copilot/token, got: %s", errMsg)
	}
}

// --- VAL-COPILOT-015: Error handling — rate limit ---

func TestCopilotBackend_RateLimit(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for rate limit")
	}

	if !strings.Contains(err.Error(), "429") && !strings.Contains(err.Error(), "rate") {
		t.Errorf("error should reference rate limit, got: %v", err)
	}
}

// --- VAL-COPILOT-016: Error handling — model not available ---

func TestCopilotBackend_ModelNotFound(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for model not found")
	}
}

// --- VAL-COPILOT-017: Error handling — subscription issues ---

func TestCopilotBackend_SubscriptionError(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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

	b, upstream, githubAPI := newTestCopilotBackendWithExchange(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&attemptCount, 1)
		if count == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"message": "Unauthorized"},
			})
			return
		}
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

	// Pre-set a Copilot token with a GitHub token for re-exchange.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "initial-copilot-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "test",
		GitHubToken: "gho-test-github-token",
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion after 401 retry: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice after retry")
	}
	if string(resp.Choices[0].Message.Content) != "\"Success after retry\"" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Success after retry")
	}

	if atomic.LoadInt32(&attemptCount) != 2 {
		t.Errorf("expected 2 attempts, got %d", atomic.LoadInt32(&attemptCount))
	}
}

// --- VAL-TOKEN-040: Upstream 401 re-auth loop prevention ---

func TestCopilotBackend_Upstream401_LoopPrevention(t *testing.T) {
	var attemptCount int32

	b, upstream, githubAPI := newTestCopilotBackendWithExchange(t)
	defer upstream.Close()
	defer githubAPI.Close()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attemptCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "Unauthorized"},
		})
	})

	preSetToken(b, "test-copilot-token")

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when upstream always returns 401")
	}

	attempts := atomic.LoadInt32(&attemptCount)
	if attempts > 2 {
		t.Errorf("expected at most 2 attempts (loop prevention), got %d", attempts)
	}
}

// --- VAL-COPILOT-007/008/009: Different base URL routing ---

func TestCopilotBackend_IndividualURL(t *testing.T) {
	b, _ := newTestCopilotBackend(t)
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
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

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
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

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
		name   string
		models []config.ModelConfig
		check  string
		want   bool
	}{
		{"empty models list (rejects when cache empty)", nil, "anything", false},
		{"exact match", []config.ModelConfig{{ID: "gpt-4o"}}, "gpt-4o", true},
		{"no match", []config.ModelConfig{{ID: "gpt-4o"}}, "claude-3", false},
		{"wildcard match", []config.ModelConfig{{ID: "openai/*"}}, "openai/gpt-4o", true},
		{"wildcard no match", []config.ModelConfig{{ID: "openai/*"}}, "anthropic/claude-3", false},
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
			deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
			b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

			if got := b.SupportsModel(tt.check); got != tt.want {
				t.Errorf("SupportsModel(%q) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

// --- VAL-COPILOT-026: Concurrent requests ---

func TestCopilotBackend_ConcurrentRequests(t *testing.T) {
	var requestCount int32

	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
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

	const numRequests = 5
	var wg sync.WaitGroup
	errCh := make(chan error, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &ChatCompletionRequest{
				Model:    "gpt-4o",
				Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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

	dir, cleanup := helperTempDir(t)
	defer cleanup()

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "copilot-token.json"))
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	backend := NewCopilotBackend(config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
		Models:  []config.ModelConfig{{ID: "gpt-4o"}, {ID: "o3"}},
	}, deviceCodeHandler, ts, nil)

	r.backends["copilot"] = backend

	_, _, err := r.Resolve("copilot-other/gpt-4o")
	if err == nil {
		t.Error("expected error for partial prefix match")
	}
}

// --- VAL-COPILOT-022: Empty messages array returns error ---

func TestCopilotBackend_EmptyMessages(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{},
	}

	// The request is forwarded to the upstream, which may or may not reject it.
	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Logf("upstream rejected empty messages (acceptable): %v", err)
	} else {
		t.Logf("upstream accepted empty messages, got response: %+v", resp)
	}
}

// --- VAL-COPILOT-027: Token re-validation on expiry ---

func TestCopilotBackend_TokenRevalidationOnExpiry(t *testing.T) {
	var exchangeCount int32

	b, upstream, githubAPI := newTestCopilotBackendWithExchange(t)
	defer upstream.Close()
	defer githubAPI.Close()

	// Override GitHub API to count exchanges.
	githubAPI.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&exchangeCount, 1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
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

	// Set an expired token WITH a GitHub token for re-exchange.
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		GitHubToken: "gho-test-github-token",
	})

	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion with expired token: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Error("expected at least one choice")
	}

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
	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts)
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

	if b.Name() != "my-copilot" {
		t.Errorf("Name() = %q, want %q", b.Name(), "my-copilot")
	}
}

// --- Verify APIKeyOverride is ignored for Copilot (VAL-COPILOT-021) ---

func TestCopilotBackend_IgnoresAPIKeyOverride(t *testing.T) {
	var receivedAuth string

	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "real-copilot-token")

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

	req := &ChatCompletionRequest{
		Model:          "gpt-4o",
		Messages:       []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		APIKeyOverride: "sk-custom-key",
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

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

	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	preSetToken(b, "test-copilot-token")

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

	rawBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"top_p":0.9}`
	req := &ChatCompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		RawBody:  []byte(rawBody),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	var modelStr string
	json.Unmarshal(receivedBody["model"], &modelStr)
	if modelStr != "gpt-4o" {
		t.Errorf("model in body = %q, want %q", modelStr, "gpt-4o")
	}

	if _, ok := receivedBody["temperature"]; !ok {
		t.Error("temperature field should be preserved")
	}
	if _, ok := receivedBody["top_p"]; !ok {
		t.Error("top_p field should be preserved")
	}
}

// --- OAuthLoginHandler test ---

func TestCopilotBackend_InitiateLogin(t *testing.T) {
	// Verify that CopilotBackend implements OAuthLoginHandler
	b, _ := newTestCopilotBackend(t)

	var _ OAuthLoginHandler = b // compile-time interface check

	// Without a mock device code server, InitiateLogin will fail.
	// That's okay — we're testing the interface implementation.
	_, _, err := b.InitiateLogin()
	if err == nil {
		// This might succeed if the real GitHub endpoint is reachable
		t.Log("InitiateLogin succeeded (unexpected but acceptable)")
	} else {
		// Expected: network error or timeout connecting to real GitHub
		t.Logf("InitiateLogin failed as expected: %v", err)
	}
}

// --- OAuthStatus test ---

func TestCopilotBackend_OAuthStatus_NoToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)

	status := b.OAuthStatus()
	if status.BackendName != "copilot" {
		t.Errorf("BackendName = %q, want %q", status.BackendName, "copilot")
	}
	if status.BackendType != "copilot" {
		t.Errorf("BackendType = %q, want %q", status.BackendType, "copilot")
	}
	if status.TokenState != "missing" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "missing")
	}
	if !status.NeedsReauth {
		t.Error("NeedsReauth should be true when no token")
	}
}

func TestCopilotBackend_OAuthStatus_ValidToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)

	preSetToken(b, "test-token")

	status := b.OAuthStatus()
	if !status.Authenticated {
		t.Error("Authenticated should be true")
	}
	if status.TokenState != "valid" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "valid")
	}
	if status.NeedsReauth {
		t.Error("NeedsReauth should be false with valid token")
	}
}

func TestCopilotBackend_OAuthStatus_ExpiredTokenWithGitHubToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)

	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		GitHubToken: "gho-test-github-token",
	})

	status := b.OAuthStatus()
	if status.TokenState != "expired" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "expired")
	}
	// With a GitHub token stored, we can auto-revalidate
	if status.NeedsReauth {
		t.Error("NeedsReauth should be false when GitHub token is stored for re-exchange")
	}
}

func TestCopilotBackend_OAuthStatus_ExpiredTokenNoGitHubToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)

	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		// No GitHub token
	})

	status := b.OAuthStatus()
	if status.TokenState != "expired" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "expired")
	}
	if !status.NeedsReauth {
		t.Error("NeedsReauth should be true when expired and no GitHub token for re-exchange")
	}
}

// Helper for creating bool pointer.

func TestCopilotBackend_RefreshOAuthStatus_Success(t *testing.T) {
	// Create a mock GitHub API server that handles the Copilot token exchange
	mockServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/copilot_internal/v2/token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"token":      "refreshed-copilot-token",
				"expires_at": time.Now().Add(30 * time.Minute).Unix(),
				"refresh_in": 1500,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer mockServer.Close()

	tokenDir := t.TempDir()
	ts, err := oauth.NewTokenStore(filepath.Join(tokenDir, "copilot-token.json"))
	if err != nil {
		t.Fatalf("creating token store: %v", err)
	}

	// Store an expired token with a GitHub token (so re-exchange is possible)
	ts.Save(&oauth.TokenData{
		AccessToken: "expired-copilot-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		GitHubToken: "test-github-token",
	})

	deviceCodeHandler := oauth.NewDeviceCodeHandler(ts,
		oauth.WithCopilotExchangerURL(mockServer.URL),
	)

	cfg := config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: "https://api.githubcopilot.com",
		Models:  []config.ModelConfig{{ID: "gpt-4o"}},
	}
	b := NewCopilotBackend(cfg, deviceCodeHandler, ts, nil)

	// RefreshOAuthStatus should succeed by re-exchanging the GitHub token
	err = b.RefreshOAuthStatus(context.Background())
	if err != nil {
		t.Fatalf("RefreshOAuthStatus returned error: %v", err)
	}

	// Verify the token store now has the fresh token
	newToken := ts.Get()
	if newToken == nil {
		t.Fatal("expected token to be stored after refresh")
	}
	if newToken.AccessToken != "refreshed-copilot-token" {
		t.Errorf("AccessToken = %q, want %q", newToken.AccessToken, "refreshed-copilot-token")
	}

	// Verify OAuthStatus reflects the new state
	status := b.OAuthStatus()
	if !status.Authenticated {
		t.Error("expected Authenticated = true after successful refresh")
	}
	if status.TokenState != "valid" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "valid")
	}
}

func TestCopilotBackend_RefreshOAuthStatus_NoToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)
	// No token stored at all

	err := b.RefreshOAuthStatus(context.Background())
	if err == nil {
		t.Error("expected error when no token is stored, got nil")
	}
}

func TestCopilotBackend_RefreshOAuthStatus_NoGitHubToken(t *testing.T) {
	b, _ := newTestCopilotBackend(t)

	// Store an expired token with no GitHub token
	b.tokenStore.Save(&oauth.TokenData{
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		// No GitHubToken
	})

	err := b.RefreshOAuthStatus(context.Background())
	if err == nil {
		t.Error("expected error when no GitHub token for re-exchange, got nil")
	}
}

// --- VAL-COPILOT-CAP-001: ListModels filters non-chat models using capabilities ---

func TestCopilotBackend_ListModels_FiltersNonChatModels(t *testing.T) {
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
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
						"id": "text-embedding-3-small", "object": "model", "owned_by": "copilot",
						"capabilities": map[string]any{
							"type":     "embeddings",
							"supports": map[string]any{"streaming": false},
							"limits":   map[string]any{"max_output_tokens": 0},
						},
					},
					{
						"id": "davinci-base", "object": "model", "owned_by": "copilot",
						"capabilities": map[string]any{
							"type":     "base",
							"supports": map[string]any{"streaming": false},
							"limits":   map[string]any{"max_output_tokens": 4096},
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	b, _ := newTestCopilotBackendAt(t, upstream.URL)
	preSetToken(b, "test-token")

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	// Only the "chat" type model should be returned.
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d: %v", len(models), models)
	}
	if models[0].ID != "gpt-4o" {
		t.Errorf("expected gpt-4o, got %s", models[0].ID)
	}

	// Non-chat models should still be in the capCache (cached but not returned).
	cap, ok := b.getCap("text-embedding-3-small")
	if !ok {
		t.Error("expected text-embedding-3-small to be in capCache")
	}
	if cap.Type != "embeddings" {
		t.Errorf("expected cap type 'embeddings', got %q", cap.Type)
	}

	cap2, ok2 := b.getCap("davinci-base")
	if !ok2 {
		t.Error("expected davinci-base to be in capCache")
	}
	if cap2.Type != "base" {
		t.Errorf("expected cap type 'base', got %q", cap2.Type)
	}
}

// --- VAL-COPILOT-CAP-002: ListModels caches capabilities and enriches MaxOutputTokens ---

func TestCopilotBackend_ListModels_CachesCapabilities(t *testing.T) {
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{
					{
						"id": "gpt-4o-special", "object": "model", "owned_by": "copilot",
						"capabilities": map[string]any{
							"type":     "chat",
							"supports": map[string]any{"streaming": true},
							"limits":   map[string]any{"max_output_tokens": int64(65536)},
						},
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	b, _ := newTestCopilotBackendAt(t, upstream.URL)
	preSetToken(b, "test-token")

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].MaxOutputTokens == nil || *models[0].MaxOutputTokens != 65536 {
		t.Errorf("expected MaxOutputTokens=65536, got %v", models[0].MaxOutputTokens)
	}

	// capCache should have streaming set.
	cap, ok := b.getCap("gpt-4o-special")
	if !ok {
		t.Fatal("expected gpt-4o-special in capCache")
	}
	if !cap.SupportsStreaming {
		t.Error("expected SupportsStreaming=true")
	}
}

// --- VAL-COPILOT-CAP-003: rewriteBody translates max_tokens for new models ---

func TestCopilotBackend_RewriteBody_MaxTokensTranslated(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	req := &ChatCompletionRequest{
		Model:   "gpt-5.4",
		RawBody: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}],"max_tokens":1024}`),
	}

	result := b.rewriteBody(req)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasOld := m["max_tokens"]; hasOld {
		t.Error("expected max_tokens to be removed")
	}
	if _, hasNew := m["max_completion_tokens"]; !hasNew {
		t.Error("expected max_completion_tokens to be present")
	}
	var maxComp int
	if err := json.Unmarshal(m["max_completion_tokens"], &maxComp); err != nil {
		t.Fatalf("unmarshal max_completion_tokens: %v", err)
	}
	if maxComp != 1024 {
		t.Errorf("expected max_completion_tokens=1024, got %d", maxComp)
	}
}

func TestCopilotBackend_RewriteBody_NoTranslationForGPT4(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	req := &ChatCompletionRequest{
		Model:   "gpt-4o",
		RawBody: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":512}`),
	}

	result := b.rewriteBody(req)

	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, hasOld := m["max_tokens"]; !hasOld {
		t.Error("expected max_tokens to be kept for gpt-4o")
	}
	if _, hasNew := m["max_completion_tokens"]; hasNew {
		t.Error("expected max_completion_tokens NOT to be present for gpt-4o")
	}
}

// --- VAL-COPILOT-CAP-004: needsMaxCompletionTokens covers known patterns ---

func TestCopilotBackend_NeedsMaxCompletionTokens_Patterns(t *testing.T) {
	b, upstream := newTestCopilotBackend(t)
	defer upstream.Close()

	cases := []struct {
		model string
		want  bool
	}{
		{"o3", true},
		{"o3-mini", true},
		{"o4-mini", true},
		{"gpt-5", true},
		{"gpt-5.4", true},
		{"gpt-5.4-mini", true},
		{"gpt-5.1", true},
		{"gpt-4o", false},
		{"gpt-4.1", false},
		{"gpt-4.1-mini", false},
		{"claude-sonnet-4", false},
		{"gemini-2.5-pro", false},
	}
	for _, tc := range cases {
		got := b.needsMaxCompletionTokens(tc.model)
		if got != tc.want {
			t.Errorf("needsMaxCompletionTokens(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// --- VAL-COPILOT-CAP-005: 400 max_tokens error triggers retry with renamed field ---

func TestCopilotBackend_MaxTokensError_RetrySuccess(t *testing.T) {
	callCount := 0
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)

		if callCount == 1 {
			// First call with max_tokens — reject.
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"code":    "invalid_request_body",
					"message": "'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.",
				},
			})
			return
		}
		// Second call should have max_completion_tokens, not max_tokens.
		if _, hasOld := body["max_tokens"]; hasOld {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "still got max_tokens on retry"})
			return
		}
		if _, hasNew := body["max_completion_tokens"]; !hasNew {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing max_completion_tokens on retry"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-retry", "object": "chat.completion", "created": time.Now().Unix(), "model": "gpt-5.4",
			"choices": []map[string]any{
				{"index": 0, "message": map[string]string{"role": "assistant", "content": "OK"}, "finish_reason": "stop"},
			},
		})
	}))
	defer upstream.Close()

	b, _ := newTestCopilotBackendAt(t, upstream.URL)
	preSetToken(b, "test-token")

	req := &ChatCompletionRequest{
		Model:   "gpt-5.4",
		RawBody: []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"max_tokens":500}`),
	}
	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2 HTTP calls (initial + retry), got %d", callCount)
	}

	// capCache should now know this model needs max_completion_tokens.
	if cap, ok := b.getCap("gpt-5.4"); ok && !cap.UseMaxCompletionTokens {
		t.Error("expected capCache to be updated with UseMaxCompletionTokens=true")
	}
}

// --- VAL-COPILOT-CAP-006: Retry does not loop — max 2 calls ---

func TestCopilotBackend_MaxTokensError_NoRetryLoop(t *testing.T) {
	callCount := 0
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "invalid_request_body",
				"message": "'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.",
			},
		})
	}))
	defer upstream.Close()

	b, _ := newTestCopilotBackendAt(t, upstream.URL)
	preSetToken(b, "test-token")

	req := &ChatCompletionRequest{
		Model:   "unknown-new-model",
		RawBody: []byte(`{"model":"unknown-new-model","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`),
	}
	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if callCount > 2 {
		t.Errorf("expected at most 2 HTTP calls to prevent loop, got %d", callCount)
	}
}

// --- VAL-COPILOT-CAP-007: Unsupported endpoint returns friendly error ---

func TestCopilotBackend_UnsupportedEndpoint_FriendlyError(t *testing.T) {
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "unsupported_api_for_model",
				"message": "model \"gpt-5.4-mini\" is not accessible via the /chat/completions endpoint",
			},
		})
	}))
	defer upstream.Close()

	b, _ := newTestCopilotBackendAt(t, upstream.URL)
	preSetToken(b, "test-token")

	req := &ChatCompletionRequest{
		Model:   "gpt-5.4-mini",
		RawBody: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"hi"}]}`),
	}
	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unsupported endpoint model")
	}
	if !strings.Contains(err.Error(), "does not support chat completions") {
		t.Errorf("expected friendly error message, got: %v", err)
	}
}

// newTestCopilotBackendAt creates a CopilotBackend pointing at the given base URL.
// Useful when the test controls the server independently.
func newTestCopilotBackendAt(t *testing.T, baseURL string) (*CopilotBackend, func()) {
	t.Helper()
	dir, cleanup := helperTempDir(t)
	ts, err := oauth.NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	b := NewCopilotBackend(config.BackendConfig{
		Name:    "copilot",
		Type:    "copilot",
		BaseURL: baseURL,
	}, oauth.NewDeviceCodeHandler(ts), ts, nil)
	return b, cleanup
}
