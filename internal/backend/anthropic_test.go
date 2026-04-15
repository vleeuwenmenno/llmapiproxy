package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	}, 0, nil)

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
	}, 0, nil)

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
	}, 0, nil)

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
	}, time.Minute, nil)

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
	}, 0, nil)

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
	}, 0, nil)

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

func TestAnthropicBackend_ChatCompletion_ToolCallsRequest(t *testing.T) {
	var received struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
		} `json:"tool_choice,omitempty"`
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
			"model": "claude-sonnet-4",
			"content": []map[string]any{
				{"type": "text", "text": "done"},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "test",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0, nil)

	// Simulate a conversation with tool_calls and tool results.
	rawBody := `{
		"model": "claude-sonnet-4",
		"messages": [
			{"role": "user", "content": "What is the weather in NYC?"},
			{"role": "assistant", "content": "", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"location\":\"NYC\"}"}}]},
			{"role": "tool", "tool_call_id": "call_1", "content": "72°F and sunny"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather info", "parameters": {"type": "object", "properties": {"location": {"type": "string"}}}}}],
		"tool_choice": "auto"
	}`

	_, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model: "claude-sonnet-4",
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"What is the weather in NYC?"`)},
			{Role: "assistant", Content: json.RawMessage(`""`)},
			{Role: "tool", Content: json.RawMessage(`"72°F and sunny"`)},
		},
		RawBody: []byte(rawBody),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify messages were translated correctly.
	// Expected: user, assistant(tool_use), user(tool_result)
	if len(received.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(received.Messages))
	}

	if received.Messages[0].Role != "user" {
		t.Fatalf("msg[0].role = %q, want user", received.Messages[0].Role)
	}
	if received.Messages[1].Role != "assistant" {
		t.Fatalf("msg[1].role = %q, want assistant", received.Messages[1].Role)
	}
	// Verify assistant message contains tool_use block.
	var assistantBlocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(received.Messages[1].Content, &assistantBlocks); err != nil {
		t.Fatalf("unmarshal assistant content: %v", err)
	}
	foundToolUse := false
	for _, block := range assistantBlocks {
		if block.Type == "tool_use" && block.ID == "call_1" && block.Name == "get_weather" {
			foundToolUse = true
		}
	}
	if !foundToolUse {
		t.Fatalf("assistant message missing tool_use block, got: %s", received.Messages[1].Content)
	}

	// Verify tool result is sent as user message with tool_result content.
	if received.Messages[2].Role != "user" {
		t.Fatalf("msg[2].role = %q, want user", received.Messages[2].Role)
	}
	var toolResultBlocks []struct {
		Type      string `json:"type"`
		ToolUseID string `json:"tool_use_id"`
	}
	if err := json.Unmarshal(received.Messages[2].Content, &toolResultBlocks); err != nil {
		t.Fatalf("unmarshal tool result content: %v", err)
	}
	if len(toolResultBlocks) != 1 || toolResultBlocks[0].Type != "tool_result" {
		t.Fatalf("expected tool_result block, got: %s", received.Messages[2].Content)
	}
	if toolResultBlocks[0].ToolUseID != "call_1" {
		t.Fatalf("tool_use_id = %q, want call_1", toolResultBlocks[0].ToolUseID)
	}

	// Verify tools were translated.
	if len(received.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(received.Tools))
	}
	if received.Tools[0].Name != "get_weather" {
		t.Fatalf("tool name = %q, want get_weather", received.Tools[0].Name)
	}

	// Verify tool_choice was translated.
	if received.ToolChoice == nil {
		t.Fatal("tool_choice is nil, want auto")
	}
	if received.ToolChoice.Type != "auto" {
		t.Fatalf("tool_choice.type = %q, want auto", received.ToolChoice.Type)
	}
}

func TestAnthropicBackend_ChatCompletion_ToolUseResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "msg_test",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-sonnet-4",
			"content": []map[string]any{
				{"type": "text", "text": "Let me check the weather."},
				{
					"type":  "tool_use",
					"id":    "toolu_01A",
					"name":  "get_weather",
					"input": map[string]any{"location": "NYC"},
				},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens":  15,
				"output_tokens": 25,
			},
		})
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "test",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0, nil)

	resp, err := b.ChatCompletion(context.Background(), &ChatCompletionRequest{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"What is the weather?"`)}},
		RawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"What is the weather?"}]}`),
	})
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}

	choice := resp.Choices[0]

	// Check text content.
	var content string
	if err := json.Unmarshal(choice.Message.Content, &content); err != nil {
		t.Fatalf("unmarshal content: %v", err)
	}
	if content != "Let me check the weather." {
		t.Fatalf("content = %q, want 'Let me check the weather.'", content)
	}

	// Check tool_calls.
	if len(choice.Message.ToolCalls) == 0 {
		t.Fatal("tool_calls is empty, want 1")
	}
	var toolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(choice.Message.ToolCalls, &toolCalls); err != nil {
		t.Fatalf("unmarshal tool_calls: %v", err)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(toolCalls))
	}
	if toolCalls[0].ID != "toolu_01A" {
		t.Fatalf("tool_call id = %q, want toolu_01A", toolCalls[0].ID)
	}
	if toolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("tool_call name = %q, want get_weather", toolCalls[0].Function.Name)
	}
	if toolCalls[0].Type != "function" {
		t.Fatalf("tool_call type = %q, want function", toolCalls[0].Type)
	}

	// Check finish_reason is "tool_calls" (OpenAI convention).
	if choice.FinishReason == nil || *choice.FinishReason != "tool_calls" {
		got := "<nil>"
		if choice.FinishReason != nil {
			got = *choice.FinishReason
		}
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
}

func TestAnthropicBackend_ChatCompletion_StreamToolUse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)

		events := []string{
			`event: message_start` + "\n" +
				`data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","model":"claude-sonnet-4","content":[]}}`,
			`event: content_block_start` + "\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_stream_1","name":"get_weather"}}`,
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`,
			`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ation\":\"NYC\"}"}}`,
			`event: content_block_stop` + "\n" +
				`data: {"type":"content_block_stop","index":0}`,
			`event: message_delta` + "\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
			`event: message_stop` + "\n" +
				`data: {"type":"message_stop"}`,
		}

		for _, e := range events {
			w.Write([]byte(e + "\n\n"))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	b := NewAnthropic(config.BackendConfig{
		Name:    "test",
		Type:    "anthropic",
		BaseURL: srv.URL,
		APIKey:  "test-key",
	}, 0, nil)

	req := &ChatCompletionRequest{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"weather?"`)}},
		Stream:   true,
		RawBody:  []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"weather?"}],"stream":true}`),
	}

	stream, err := b.ChatCompletionStream(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	defer stream.Close()

	// Read all the streamed SSE data.
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("ReadAll stream: %v", err)
	}
	body := string(data)

	// Verify the stream contains tool_call chunks.
	if !strings.Contains(body, "tool_calls") {
		t.Fatalf("stream does not contain tool_calls chunk:\n%s", body)
	}
	if !strings.Contains(body, "toolu_stream_1") {
		t.Fatalf("stream does not contain tool call ID:\n%s", body)
	}
	if !strings.Contains(body, "get_weather") {
		t.Fatalf("stream does not contain tool function name:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream does not contain [DONE]:\n%s", body)
	}
}
