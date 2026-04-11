package backend

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
)

// codexTestHelper sets up a CodexBackend with mock upstream and OAuth servers.
// Returns the backend, the upstream server, and cleanup function.
func codexTestHelper(t *testing.T) (*CodexBackend, *httptest.Server, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "codex-test-*")
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() { os.RemoveAll(dir) }

	// Create token store.
	ts, err := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	if err != nil {
		cleanup()
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Create a mock OAuth token server.
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "test-access-token-refreshed",
			"refresh_token": "test-refresh-token-new",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email offline_access",
		})
	}))

	// Create CodexOAuthHandler with mock token server.
	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthCfg.TokenURL = tokenServer.URL
	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	// Create mock Codex upstream (Responses API).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-123",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-123",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type": "output_text",
							"text": "Hello from Codex!",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  15,
				"output_tokens": 8,
				"total_tokens":  23,
			},
		})
	}))

	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: upstream.URL,
		Models:  []string{"o4-mini", "gpt-5.2-codex"},
	}

	b := NewCodexBackend(cfg, oauthHandler, ts)

	// Pre-set a valid access token.
	ts.Save(&oauth.TokenData{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "test",
	})

	fullCleanup := func() {
		upstream.Close()
		tokenServer.Close()
		cleanup()
	}

	return b, upstream, fullCleanup
}

// --- VAL-CODEX-001: Non-streaming chat completion routes to Codex backend ---

func TestCodexBackend_ChatCompletion(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model: "o4-mini",
		Messages: []Message{
			{Role: "user", Content: "Hello"},
		},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
	}
	if resp.ID != "resp-test-123" {
		t.Errorf("id = %q, want %q", resp.ID, "resp-test-123")
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].Message == nil {
		t.Fatal("choice message is nil")
	}
	if resp.Choices[0].Message.Content != "Hello from Codex!" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello from Codex!")
	}
	if resp.Choices[0].Message.Role != "assistant" {
		t.Errorf("role = %q, want %q", resp.Choices[0].Message.Role, "assistant")
	}
}

// --- VAL-CODEX-025: Format translation — response ID and metadata preserved ---

func TestCodexBackend_ResponseMetadata(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.ID == "" {
		t.Error("id should not be empty")
	}
	if resp.Created == 0 {
		t.Error("created should not be zero")
	}
	if resp.Model == "" {
		t.Error("model should not be empty")
	}
}

// --- VAL-CODEX-026: Format translation — usage stats are populated ---

func TestCodexBackend_UsageStats(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
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

// --- VAL-CODEX-027: Format translation — finish_reason is set correctly ---

func TestCodexBackend_FinishReason_Stop(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if resp.Choices[0].FinishReason == nil {
		t.Fatal("finish_reason is nil")
	}
	if *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want %q", *resp.Choices[0].FinishReason, "stop")
	}
}

func TestCodexBackend_FinishReason_Length(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	// Return incomplete response (simulates max_tokens truncation).
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-truncated",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "incomplete",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-truncated",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{
							"type": "output_text",
							"text": "Truncated...",
						},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  15,
				"output_tokens": 5,
				"total_tokens":  20,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want %q", resp.Choices[0].FinishReason, "length")
	}
}

// --- VAL-CODEX-002: Multi-turn conversation translation ---

func TestCodexBackend_MultiTurn(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedBody map[string]json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-multi",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-multi",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Multi-turn response"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  50,
				"output_tokens": 10,
				"total_tokens":  60,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model: "o4-mini",
		Messages: []Message{
			{Role: "system", Content: "You are a helpful assistant."},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
			{Role: "user", Content: "Tell me more"},
		},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if len(resp.Choices) == 0 || resp.Choices[0].Message == nil {
		t.Fatal("expected at least one choice with message")
	}

	// Verify the input was properly translated.
	// The system message should have been extracted as "instructions".
	if instructionsRaw, ok := receivedBody["instructions"]; ok {
		var instructions string
		json.Unmarshal(instructionsRaw, &instructions)
		if instructions != "You are a helpful assistant." {
			t.Errorf("instructions = %q, want system message", instructions)
		}
	}

	// The input should contain the non-system messages.
	if inputRaw, ok := receivedBody["input"]; ok {
		var msgs []codexInputMessage
		if err := json.Unmarshal(inputRaw, &msgs); err == nil {
			// Should have 3 messages: user, assistant, user
			if len(msgs) != 3 {
				t.Errorf("expected 3 input messages, got %d", len(msgs))
			}
		}
	}
}

// --- VAL-CODEX-003: Temperature and max_tokens translation ---

func TestCodexBackend_TemperatureAndMaxTokens(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedBody map[string]json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-params",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-params",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Response"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"total_tokens":  15,
			},
		})
	})

	temp := 0.7
	maxTokens := 500
	req := &ChatCompletionRequest{
		Model:       "o4-mini",
		Messages:    []Message{{Role: "user", Content: "test"}},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify temperature was forwarded.
	if tempRaw, ok := receivedBody["temperature"]; ok {
		var gotTemp float64
		json.Unmarshal(tempRaw, &gotTemp)
		if gotTemp != 0.7 {
			t.Errorf("temperature = %f, want 0.7", gotTemp)
		}
	} else {
		t.Error("temperature not found in forwarded request")
	}

	// Verify max_output_tokens was set (mapped from max_tokens).
	if maxRaw, ok := receivedBody["max_output_tokens"]; ok {
		var gotMax int
		json.Unmarshal(maxRaw, &gotMax)
		if gotMax != 500 {
			t.Errorf("max_output_tokens = %d, want 500", gotMax)
		}
	} else {
		t.Error("max_output_tokens not found in forwarded request")
	}
}

// --- VAL-CODEX-004: Streaming produces valid SSE event stream ---

func TestCodexBackend_ChatCompletionStream(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		events := []string{
			`{"type":"response.created","response":{"id":"resp-stream-1","object":"response","created_at":1234,"status":"in_progress","model":"o4-mini","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg-1","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":"Hello"}`,
			`{"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":" from"}`,
			`{"type":"response.output_text.delta","item_id":"msg-1","output_index":0,"content_index":0,"delta":" Codex!"}`,
			`{"type":"response.output_text.done","item_id":"msg-1","output_index":0,"content_index":0,"text":"Hello from Codex!"}`,
			`{"type":"response.completed","response":{"id":"resp-stream-1","object":"response","created_at":1234,"status":"completed","model":"o4-mini","output":[{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello from Codex!"}]}],"usage":{"input_tokens":15,"output_tokens":8,"total_tokens":23}}}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "event: %s\n", json.RawMessage(event))
			fmt.Fprintf(w, "data: %s\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
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

	bodyStr := string(body)

	// Should contain ChatCompletion-format SSE chunks.
	if !strings.Contains(bodyStr, "data: ") {
		t.Error("expected SSE data lines in stream")
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("expected [DONE] sentinel in stream")
	}
	if !strings.Contains(bodyStr, "chat.completion.chunk") {
		t.Error("expected chat.completion.chunk objects in stream")
	}
	if !strings.Contains(bodyStr, "delta") {
		t.Error("expected delta field in streaming chunks")
	}
	if !strings.Contains(bodyStr, `"stop"`) {
		t.Error("expected finish_reason: stop in stream")
	}

	// Verify delta content was translated correctly.
	if !strings.Contains(bodyStr, "Hello") {
		t.Error("expected 'Hello' in stream content")
	}
	if !strings.Contains(bodyStr, "Codex!") {
		t.Error("expected 'Codex!' in stream content")
	}
}

// --- VAL-CODEX-028: Streaming finish_reason transitions correctly ---

func TestCodexBackend_Streaming_FinishReason(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		events := []string{
			`{"type":"response.output_text.delta","delta":"Hi","item_id":"msg-1","output_index":0,"content_index":0}`,
			`{"type":"response.output_text.delta","delta":" there","item_id":"msg-1","output_index":0,"content_index":0}`,
			`{"type":"response.completed","response":{"id":"resp-1","status":"completed","model":"o4-mini","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hi there"}]}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		}

		for _, event := range events {
			fmt.Fprintf(w, "event: %s\n", event)
			fmt.Fprintf(w, "data: %s\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	stream, err := b.ChatCompletionStream(context.Background(), &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	body, _ := io.ReadAll(stream)
	bodyStr := string(body)

	// Parse all SSE chunks and verify finish_reason transitions.
	lines := strings.Split(bodyStr, "\n")
	var hasNullFinish, hasStopFinish bool
	for _, line := range lines {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		data := line[6:]
		var chunk map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if choicesRaw, ok := chunk["choices"]; ok {
			var choices []struct {
				FinishReason *string `json:"finish_reason"`
			}
			json.Unmarshal(choicesRaw, &choices)
			for _, c := range choices {
				if c.FinishReason == nil {
					hasNullFinish = true
				} else if *c.FinishReason == "stop" {
					hasStopFinish = true
				}
			}
		}
	}

	if !hasNullFinish {
		t.Error("expected intermediate chunks with finish_reason: null")
	}
	if !hasStopFinish {
		t.Error("expected final chunk with finish_reason: stop")
	}
}

// --- VAL-CODEX-008/009: Model listing ---

func TestCodexBackend_ListModels(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	modelIDs := make(map[string]bool)
	for _, m := range models {
		modelIDs[m.ID] = true
		if m.Object != "model" {
			t.Errorf("model %q object = %q, want %q", m.ID, m.Object, "model")
		}
		if m.OwnedBy != "codex" {
			t.Errorf("model %q owned_by = %q, want %q", m.ID, m.OwnedBy, "codex")
		}
		if m.Created == 0 {
			t.Errorf("model %q has zero created timestamp", m.ID)
		}
	}

	for _, expected := range []string{"o4-mini", "gpt-5.2-codex"} {
		if !modelIDs[expected] {
			t.Errorf("expected model %q in list", expected)
		}
	}
}

func TestCodexBackend_ListModels_Default(t *testing.T) {
	dir, err := os.MkdirTemp("", "codex-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		// No models configured — should use defaults.
	}
	b := NewCodexBackend(cfg, nil, ts)

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("expected default models when none configured")
	}

	found := false
	for _, m := range models {
		if m.ID == "o4-mini" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected o4-mini in default models")
	}
}

// --- SupportsModel tests ---

func TestCodexBackend_SupportsModel(t *testing.T) {
	tests := []struct {
		name   string
		models []string
		check  string
		want   bool
	}{
		{"empty models list (accepts all)", nil, "anything", true},
		{"exact match", []string{"o4-mini"}, "o4-mini", true},
		{"no match", []string{"o4-mini"}, "gpt-5", false},
		{"wildcard match", []string{"gpt-5/*"}, "gpt-5/codex", true},
		{"wildcard no match", []string{"gpt-5/*"}, "o3/mini", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.BackendConfig{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				Models:  tt.models,
			}
			dir, _ := os.MkdirTemp("", "codex-test-*")
			defer os.RemoveAll(dir)

			ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
			b := NewCodexBackend(cfg, nil, ts)

			if got := b.SupportsModel(tt.check); got != tt.want {
				t.Errorf("SupportsModel(%q) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

// --- VAL-CODEX-018: Error when tokens are unavailable ---

func TestCodexBackend_NoTokens(t *testing.T) {
	dir, err := os.MkdirTemp("", "codex-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	// No token saved — should fail.

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)

	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
	}
	b := NewCodexBackend(cfg, oauthHandler, ts)

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err = b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error when no token available")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "authentication") && !strings.Contains(errMsg, "OAuth") {
		t.Errorf("error should mention authentication/OAuth, got: %s", errMsg)
	}
}

// --- VAL-CODEX-020: Rate limit error forwarding ---

func TestCodexBackend_RateLimit(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "30")
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Rate limit exceeded",
				"type":    "rate_limit_error",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for rate limit")
	}

	var be *BackendError
	if !strings.Contains(err.Error(), "429") {
		t.Errorf("error should reference 429, got: %v", err)
	}
	_ = be
}

// --- VAL-CODEX-021: Model not found error ---

func TestCodexBackend_ModelNotFound(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

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
}

// --- VAL-CODEX-022: Subscription/billing error ---

func TestCodexBackend_SubscriptionError(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Insufficient subscription quota",
				"type":    "subscription_error",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for subscription issue")
	}

	var be *BackendError
	if !strings.Contains(err.Error(), "402") {
		t.Errorf("error should reference 402, got: %v", err)
	}
	_ = be
}

// --- VAL-CODEX-023: Internal server error ---

func TestCodexBackend_ServerError(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": "Internal server error",
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for server error")
	}

	var be *BackendError
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should reference 500, got: %v", err)
	}
	_ = be
}

// --- VAL-CODEX-035: Empty messages array ---

func TestCodexBackend_EmptyMessages(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty messages")
	}

	var be *BackendError
	if !strings.Contains(err.Error(), "empty") && !strings.Contains(err.Error(), "messages") {
		t.Logf("error = %v", err)
	}
	_ = be
}

// --- VAL-CODEX-039: Tool/function calling graceful handling ---

func TestCodexBackend_ToolCalling(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	// The backend should forward the request (tools are preserved in extra fields).
	// The upstream may or may not support tools — the backend should not crash.
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-tools",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-tools",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "I can't use tools in this mode."},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 10,
				"total_tokens":  30,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "Use tool X"}},
		RawBody:  []byte(`{"model":"o4-mini","messages":[{"role":"user","content":"Use tool X"}],"tools":[{"type":"function","name":"my_func"}]}`),
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion with tools: %v", err)
	}
	if resp == nil {
		t.Fatal("response should not be nil")
	}
}

// --- VAL-CODEX-034: Extra fields preserved ---

func TestCodexBackend_ExtraFields(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedBody map[string]json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-extra",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-extra",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Extra fields test"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
				"total_tokens":  15,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
		RawBody:  []byte(`{"model":"o4-mini","messages":[{"role":"user","content":"test"}],"top_p":0.9,"presence_penalty":0.5,"frequency_penalty":0.3}`),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify extra fields were preserved in the forwarded request.
	if _, ok := receivedBody["presence_penalty"]; !ok {
		t.Error("presence_penalty should be preserved")
	}
	if _, ok := receivedBody["frequency_penalty"]; !ok {
		t.Error("frequency_penalty should be preserved")
	}
	if _, ok := receivedBody["top_p"]; !ok {
		t.Error("top_p should be preserved")
	}
}

// --- VAL-CODEX-031: Concurrent requests ---

func TestCodexBackend_ConcurrentRequests(t *testing.T) {
	var requestCount int32

	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requestCount, 1)
		time.Sleep(10 * time.Millisecond) // Simulate latency.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-concurrent",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-concurrent",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Concurrent response"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 3,
				"total_tokens":  8,
			},
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
				Model:    "o4-mini",
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

// --- VAL-CODEX-033: Concurrent streaming and non-streaming ---

func TestCodexBackend_ConcurrentMixed(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this is a streaming request.
		var body map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &body)

		isStream := false
		if streamRaw, ok := body["stream"]; ok {
			json.Unmarshal(streamRaw, &isStream)
		}

		if isStream {
			w.Header().Set("Content-Type", "text/event-stream")
			events := []string{
				`{"type":"response.output_text.delta","delta":"Stream response","item_id":"msg-1","output_index":0,"content_index":0}`,
				`{"type":"response.completed","response":{"id":"resp-mix","status":"completed","model":"o4-mini","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Stream response"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
			}
			for _, event := range events {
				fmt.Fprintf(w, "data: %s\n\n", event)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":         "resp-mix-nonstream",
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "completed",
				"model":      "o4-mini",
				"output": []map[string]any{
					{
						"type":   "message",
						"id":     "msg-mix",
						"role":   "assistant",
						"status": "completed",
						"content": []map[string]any{
							{"type": "output_text", "text": "Non-stream response"},
						},
					},
				},
				"usage": map[string]any{
					"input_tokens":  5,
					"output_tokens": 3,
					"total_tokens":  8,
				},
			})
		}
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	// Non-streaming request.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &ChatCompletionRequest{
			Model:    "o4-mini",
			Messages: []Message{{Role: "user", Content: "non-stream test"}},
		}
		resp, err := b.ChatCompletion(context.Background(), req)
		if err != nil {
			errCh <- err
			return
		}
		if len(resp.Choices) == 0 {
			errCh <- fmt.Errorf("no choices in non-streaming response")
		}
	}()

	// Streaming request.
	wg.Add(1)
	go func() {
		defer wg.Done()
		req := &ChatCompletionRequest{
			Model:    "o4-mini",
			Messages: []Message{{Role: "user", Content: "stream test"}},
		}
		stream, err := b.ChatCompletionStream(context.Background(), req)
		if err != nil {
			errCh <- err
			return
		}
		defer stream.Close()
		body, err := io.ReadAll(stream)
		if err != nil {
			errCh <- err
			return
		}
		if !strings.Contains(string(body), "data: ") {
			errCh <- fmt.Errorf("expected SSE data in streaming response")
		}
	}()

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("concurrent mixed request failed: %v", err)
	}
}

// --- VAL-CODEX-029: Coexistence with OpenAI backends ---

func TestCodexBackend_CoexistenceWithOpenAI(t *testing.T) {
	r := NewRegistry()

	dir, _ := os.MkdirTemp("", "codex-test-*")
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		Models:  []string{"o4-mini"},
	}
	b := NewCodexBackend(cfg, nil, ts)
	r.Register("codex", b)

	// Also register an OpenAI backend.
	openaiCfg := config.BackendConfig{
		Name:    "openrouter",
		Type:    "openai",
		BaseURL: "https://openrouter.ai/api/v1",
		APIKey:  "test-key",
		Models:  []string{"openai/gpt-4o"},
	}
	r.Register("openrouter", NewOpenAI(openaiCfg))

	// Verify both backends exist.
	if !r.Has("codex") {
		t.Error("codex backend should be registered")
	}
	if !r.Has("openrouter") {
		t.Error("openrouter backend should be registered")
	}

	// Verify model resolution.
	codexBackend, modelID, err := r.Resolve("codex/o4-mini")
	if err != nil {
		t.Fatalf("Resolve codex/o4-mini: %v", err)
	}
	if codexBackend.Name() != "codex" {
		t.Errorf("resolved backend = %q, want %q", codexBackend.Name(), "codex")
	}
	if modelID != "o4-mini" {
		t.Errorf("modelID = %q, want %q", modelID, "o4-mini")
	}
}

// --- VAL-CODEX-015/016: Prefix routing ---

func TestCodexBackend_PrefixRouting(t *testing.T) {
	r := NewRegistry()

	dir, _ := os.MkdirTemp("", "codex-test-*")
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		Models:  []string{"o4-mini", "gpt-5.2-codex"},
	}
	b := NewCodexBackend(cfg, nil, ts)
	r.Register("codex", b)

	// Test codex/o4-mini.
	backend, modelID, err := r.Resolve("codex/o4-mini")
	if err != nil {
		t.Fatalf("Resolve codex/o4-mini: %v", err)
	}
	if backend.Name() != "codex" {
		t.Errorf("backend = %q, want codex", backend.Name())
	}
	if modelID != "o4-mini" {
		t.Errorf("modelID = %q, want o4-mini", modelID)
	}

	// Test codex/gpt-5.2-codex.
	backend2, modelID2, err := r.Resolve("codex/gpt-5.2-codex")
	if err != nil {
		t.Fatalf("Resolve codex/gpt-5.2-codex: %v", err)
	}
	if backend2.Name() != "codex" {
		t.Errorf("backend = %q, want codex", backend2.Name())
	}
	if modelID2 != "gpt-5.2-codex" {
		t.Errorf("modelID = %q, want gpt-5.2-codex", modelID2)
	}
}

// --- VAL-CODEX-017: Prefix routing does not match non-codex prefixes ---

func TestCodexBackend_PrefixRoutingNoCrossMatch(t *testing.T) {
	r := NewRegistry()

	dir, _ := os.MkdirTemp("", "codex-test-*")
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
		Models:  []string{"o4-mini"},
	}
	b := NewCodexBackend(cfg, nil, ts)
	r.Register("codex", b)

	// openrouter/... should NOT match codex.
	_, _, err := r.Resolve("openrouter/o4-mini")
	if err == nil {
		t.Error("expected error for openrouter/o4-mini — should not match codex backend")
	}
}

// --- VAL-CODEX-042: Unicode preservation ---

func TestCodexBackend_Unicode(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedInput json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		json.Unmarshal(raw, &body)
		receivedInput = body["input"]

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-unicode",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-unicode",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "你好！🎉 Here's a café: naïve résumé"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  20,
				"output_tokens": 15,
				"total_tokens":  35,
			},
		})
	})

	unicodeContent := "Hello 你好 🎉 café naïve résumé"
	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: unicodeContent}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify unicode in response was preserved.
	if !strings.Contains(resp.Choices[0].Message.Content, "你好") {
		t.Error("Chinese characters not preserved in response")
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "🎉") {
		t.Error("Emoji not preserved in response")
	}
	if !strings.Contains(resp.Choices[0].Message.Content, "café") {
		t.Error("Accented characters not preserved in response")
	}

	// Verify unicode in input was preserved.
	if receivedInput != nil {
		inputStr := string(receivedInput)
		if !strings.Contains(inputStr, "你好") {
			t.Error("Chinese characters not preserved in input")
		}
		if !strings.Contains(inputStr, "🎉") {
			t.Error("Emoji not preserved in input")
		}
	}
}

// --- VAL-CODEX-041: Large request body ---

func TestCodexBackend_LargeRequestBody(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-large",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-large",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Large request handled"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  500,
				"output_tokens": 5,
				"total_tokens":  505,
			},
		})
	})

	// Build a large messages array (50+ messages).
	messages := make([]Message, 51)
	messages[0] = Message{Role: "system", Content: "You are helpful."}
	for i := 1; i < 51; i++ {
		if i%2 == 1 {
			messages[i] = Message{Role: "user", Content: fmt.Sprintf("User message %d", i)}
		} else {
			messages[i] = Message{Role: "assistant", Content: fmt.Sprintf("Assistant reply %d", i)}
		}
	}

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: messages,
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion with large body: %v", err)
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
}

// --- 401 retry with re-auth ---

func TestCodexBackend_Upstream401_Retry(t *testing.T) {
	var attemptCount int32

	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

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
			"id":         "resp-test-retry",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-retry",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Success after retry"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 3,
				"total_tokens":  8,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion after 401 retry: %v", err)
	}

	if resp.Choices[0].Message.Content != "Success after retry" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Success after retry")
	}

	if atomic.LoadInt32(&attemptCount) != 2 {
		t.Errorf("expected 2 attempts (initial + retry), got %d", atomic.LoadInt32(&attemptCount))
	}
}

// --- 401 loop prevention ---

func TestCodexBackend_Upstream401_LoopPrevention(t *testing.T) {
	var attemptCount int32

	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attemptCount, 1)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"message": "Unauthorized"},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
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

// --- Name() test ---

func TestCodexBackend_Name(t *testing.T) {
	cfg := config.BackendConfig{
		Name:    "my-codex",
		Type:    "codex",
		BaseURL: "https://chatgpt.com/backend-api/codex",
	}

	dir, _ := os.MkdirTemp("", "codex-test-*")
	defer os.RemoveAll(dir)

	ts, _ := oauth.NewTokenStore(filepath.Join(dir, "codex-token.json"))
	b := NewCodexBackend(cfg, nil, ts)

	if b.Name() != "my-codex" {
		t.Errorf("Name() = %q, want %q", b.Name(), "my-codex")
	}
}

// --- Disabled state ---

func TestCodexBackend_Disabled(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.BackendConfig{
			{
				Name:    "codex",
				Type:    "codex",
				BaseURL: "https://chatgpt.com/backend-api/codex",
				Enabled: boolPtr(false),
			},
		},
		Server: config.ServerConfig{
			APIKeys: []string{"test-key"},
		},
	}

	r := NewRegistry()
	r.LoadFromConfig(cfg)

	if r.Has("codex") {
		t.Error("disabled codex backend should not be registered")
	}
}

// --- Endpoint URL verification ---

func TestCodexBackend_EndpointURL(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var requestURL string
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURL = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "resp-test-url",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-url",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "URL test"},
					},
				},
			},
			"usage": map[string]any{
				"input_tokens":  5,
				"output_tokens": 3,
				"total_tokens":  8,
			},
		})
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if requestURL != "/responses" {
		t.Errorf("request URL = %q, want %q", requestURL, "/responses")
	}
}

// --- Empty stream test (edge case) ---

func TestCodexBackend_EmptyStream(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	// Upstream returns an empty stream with just response.completed.
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"id":"resp-empty","status":"completed","model":"o4-mini","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":""}]}],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	stream, err := b.ChatCompletionStream(context.Background(), &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: "test"}},
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	body, _ := io.ReadAll(stream)
	bodyStr := string(body)

	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("expected [DONE] sentinel even for empty stream")
	}
}

// Helper for creating bool pointer.

// --- OAuthStatusProvider tests ---

func TestCodexBackend_OAuthStatus_NoToken(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// Clear the pre-set token from codexTestHelper.
	b.GetTokenStore().Clear()

	status := b.OAuthStatus()
	if status.BackendName != "codex" {
		t.Errorf("BackendName = %q, want %q", status.BackendName, "codex")
	}
	if status.BackendType != "codex" {
		t.Errorf("BackendType = %q, want %q", status.BackendType, "codex")
	}
	if status.Authenticated {
		t.Error("should not be authenticated without token")
	}
	if !status.NeedsReauth {
		t.Error("should need re-auth without token")
	}
}

func TestCodexBackend_OAuthStatus_WithValidToken(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// Save a valid token.
	b.GetTokenStore().Save(&oauth.TokenData{
		AccessToken:  "valid-access-token",
		RefreshToken: "valid-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	})

	status := b.OAuthStatus()
	if !status.Authenticated {
		t.Error("should be authenticated with valid token")
	}
	if status.NeedsReauth {
		t.Error("should not need re-auth with valid token")
	}
	if status.TokenSource != "codex_oauth" {
		t.Errorf("TokenSource = %q, want %q", status.TokenSource, "codex_oauth")
	}
	if status.TokenExpiry == "" {
		t.Error("TokenExpiry should be set")
	}
}

func TestCodexBackend_OAuthStatus_WithExpiredToken(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// Save an expired token.
	b.GetTokenStore().Save(&oauth.TokenData{
		AccessToken:  "expired-access-token",
		RefreshToken: "valid-refresh-token",
		TokenType:    "Bearer",
		ExpiresAt:    time.Now().Add(-1 * time.Hour),
		ObtainedAt:   time.Now().Add(-2 * time.Hour),
		Source:       "codex_oauth",
	})

	status := b.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated with expired token")
	}
	// Has refresh token, so doesn't need re-auth yet (can refresh).
	if status.NeedsReauth {
		t.Error("should not need re-auth when refresh token exists")
	}
}

func TestCodexBackend_OAuthStatus_WithExpiredTokenNoRefresh(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// Save an expired token without refresh token.
	b.GetTokenStore().Save(&oauth.TokenData{
		AccessToken: "expired-access-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "codex_oauth",
	})

	status := b.OAuthStatus()
	if status.Authenticated {
		t.Error("should not be authenticated with expired token")
	}
	if !status.NeedsReauth {
		t.Error("should need re-auth when token expired without refresh token")
	}
}

func TestCodexBackend_InitiateLogin(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	authURL, state, err := b.InitiateLogin()
	if err != nil {
		t.Fatalf("InitiateLogin failed: %v", err)
	}
	if authURL == "" {
		t.Error("authURL should not be empty")
	}
	if state == "" {
		t.Error("state should not be empty")
	}
	if !strings.Contains(authURL, "code_challenge") {
		t.Error("authURL should contain code_challenge parameter")
	}
	if !strings.Contains(authURL, state) {
		t.Error("authURL should contain state parameter")
	}
}

func TestCodexBackend_Disconnect(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// Save a token first.
	b.GetTokenStore().Save(&oauth.TokenData{
		AccessToken:  "test-token",
		RefreshToken: "test-refresh",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		ObtainedAt:   time.Now(),
		Source:       "codex_oauth",
	})

	if b.GetTokenStore().Get() == nil {
		t.Fatal("token should exist before disconnect")
	}

	if err := b.Disconnect(); err != nil {
		t.Fatalf("Disconnect failed: %v", err)
	}

	if b.GetTokenStore().Get() != nil {
		t.Error("token should be nil after disconnect")
	}
}

func TestCodexBackend_HandleCallback(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	// First initiate login to get a state.
	_, state, err := b.InitiateLogin()
	if err != nil {
		t.Fatalf("InitiateLogin failed: %v", err)
	}

	// Handle the callback (the mock token server will respond).
	err = b.HandleCallback(context.Background(), "test-auth-code", state)
	if err != nil {
		t.Fatalf("HandleCallback failed: %v", err)
	}

	// Verify token was stored.
	token := b.GetTokenStore().Get()
	if token == nil {
		t.Fatal("token should be stored after callback")
	}
	if token.AccessToken != "test-access-token-refreshed" {
		t.Errorf("AccessToken = %q, want %q", token.AccessToken, "test-access-token-refreshed")
	}
}

func TestCodexBackend_HandleCallback_InvalidState(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	err := b.HandleCallback(context.Background(), "test-code", "invalid-state")
	if err == nil {
		t.Error("expected error for invalid state")
	}
}
