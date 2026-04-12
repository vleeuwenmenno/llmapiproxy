package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
)

func TestAnthropicBackend_ChatCompletion(t *testing.T) {
	var received map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer test-key")
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Fatalf("x-api-key = %q, want %q", got, "test-key")
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicDefaultVersion {
			t.Fatalf("anthropic-version = %q, want %q", got, anthropicDefaultVersion)
		}

		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &received); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-sonnet-4",
			"content": []map[string]any{
				{"type": "text", "text": "Hello from Anthropic"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0)

	maxTokens := 256
	temp := 0.4
	req := &ChatCompletionRequest{
		Model:       "claude-sonnet-4",
		Messages:    []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		MaxTokens:   &maxTokens,
		Temperature: &temp,
		RawBody:     []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}],"max_tokens":256,"temperature":0.4}`),
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if resp.Model != "claude-sonnet-4" {
		t.Fatalf("model = %q, want claude-sonnet-4", resp.Model)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if _, ok := received["messages"]; !ok {
		t.Fatal("expected anthropic messages in request")
	}
	if _, ok := received["max_tokens"]; !ok {
		t.Fatal("expected max_tokens in request")
	}
}

func TestAnthropicBackend_ChatCompletion_NestedTextBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "glm-4.7",
			"content": []map[string]any{
				{"type": "text", "text": map[string]any{"value": "Hello from nested text"}},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "zai-anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0)

	resp, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model:    "glm-4.7",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		RawBody:  []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	got := ""
	if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		if err := json.Unmarshal(resp.Choices[0].Message.Content, &got); err != nil {
			t.Fatalf("decode content: %v", err)
		}
	}
	if got != "Hello from nested text" {
		t.Fatalf("content = %q, want %q", got, "Hello from nested text")
	}
}

func TestAnthropicBackend_ChatCompletion_OpenAIFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl_test",
			"object":  "chat.completion",
			"created": 123,
			"model":   "glm-4.7",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": "Hello from fallback",
					},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "zai-anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0)

	resp, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model:    "glm-4.7",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		RawBody:  []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	got := ""
	if len(resp.Choices) > 0 && resp.Choices[0].Message != nil {
		if err := json.Unmarshal(resp.Choices[0].Message.Content, &got); err != nil {
			t.Fatalf("decode content: %v", err)
		}
	}
	if got != "Hello from fallback" {
		t.Fatalf("content = %q, want %q", got, "Hello from fallback")
	}
}

func TestAnthropicBackend_ListModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{
					"id":           "claude-sonnet-4-5",
					"type":         "model",
					"display_name": "Claude Sonnet 4.5",
					"created_at":   "2026-03-01T00:00:00Z",
				},
			},
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, time.Minute)

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("models = %d, want 1", len(models))
	}
	if models[0].ID != "claude-sonnet-4-5" {
		t.Fatalf("id = %q, want claude-sonnet-4-5", models[0].ID)
	}
	if models[0].DisplayName == "" {
		t.Fatal("expected display name")
	}
}

func TestAnthropicBackend_ChatCompletion_BaseURLWithV1PreservesPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-sonnet-4",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL + "/v1",
		APIKey:  "test-key",
	}, 0)

	resp, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		RawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hi"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
}

func TestAnthropicBackend_ChatCompletion_SkipsEmptyTextMessages(t *testing.T) {
	var received struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "glm-4.7",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "zai-anthropic",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0)

	resp, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model: "glm-4.7",
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
			{Role: "assistant", Content: json.RawMessage(`""`)},
			{Role: "user", Content: json.RawMessage(`"next"`)}},
		RawBody: []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":""},{"role":"user","content":"next"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	if len(received.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(received.Messages))
	}
	for _, msg := range received.Messages {
		if len(msg.Content) != 1 {
			t.Fatalf("content blocks = %d, want 1", len(msg.Content))
		}
		if msg.Content[0].Text == "" {
			t.Fatalf("text block missing text for role %q", msg.Role)
		}
	}
}
