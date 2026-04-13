package backend

import (
	"bytes"
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
	tokenServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	// Responds with SSE events since ChatCompletion now forces stream=true
	// internally for prompt caching consistency.
	upstream := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this is a streaming request.
		var reqBody map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &reqBody)
		isStream := false
		if streamRaw, ok := reqBody["stream"]; ok {
			json.Unmarshal(streamRaw, &isStream)
		}

		resp := map[string]any{
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
		}

		if isStream {
			codexStreamResponse(w, resp)
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
		}
	}))

	cfg := config.BackendConfig{
		Name:    "codex",
		Type:    "codex",
		BaseURL: upstream.URL,
		Models:  []config.ModelConfig{{ID: "o4-mini"}, {ID: "gpt-5.2-codex"}},
	}

	b := NewCodexBackend(cfg, oauthHandler, ts, nil, 0)

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

// codexStreamResponse wraps a non-streaming JSON response as an SSE stream.
// This is used in tests now that ChatCompletion forces stream=true internally.
func codexStreamResponse(w http.ResponseWriter, response map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")

	// Extract text from the response for delta events.
	text := ""
	if output, ok := response["output"].([]map[string]any); ok && len(output) > 0 {
		if content, ok := output[0]["content"].([]map[string]any); ok && len(content) > 0 {
			text, _ = content[0]["text"].(string)
		}
	}

	// Send text delta events.
	if text != "" {
		deltaEvent := map[string]any{
			"type":          "response.output_text.delta",
			"delta":         text,
			"item_id":       "msg-test",
			"output_index":  0,
			"content_index": 0,
		}
		deltaJSON, _ := json.Marshal(deltaEvent)
		fmt.Fprintf(w, "data: %s\n\n", deltaJSON)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	// Send response.completed event with the full response.
	completedEvent := map[string]any{
		"type":     "response.completed",
		"response": response,
	}
	completedJSON, _ := json.Marshal(completedEvent)
	fmt.Fprintf(w, "data: %s\n\n", completedJSON)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// codexRespondWithJSON sends a response as SSE if the request has stream=true,
// or as plain JSON otherwise. Used for test handlers that need to support both modes.
func codexRespondWithJSON(w http.ResponseWriter, r *http.Request, response map[string]any) {
	// Detect if the request has stream=true.
	var reqBody map[string]json.RawMessage
	raw, _ := io.ReadAll(r.Body)
	json.Unmarshal(raw, &reqBody)
	isStream := false
	if streamRaw, ok := reqBody["stream"]; ok {
		json.Unmarshal(streamRaw, &isStream)
	}
	// Re-read the body since we consumed it.
	r.Body = io.NopCloser(bytes.NewReader(raw))

	if isStream {
		codexStreamResponse(w, response)
	} else {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	}
}

// --- VAL-CODEX-001: Non-streaming chat completion routes to Codex backend ---

func TestCodexBackend_ChatCompletion(t *testing.T) {
	b, _, cleanup := codexTestHelper(t)
	defer cleanup()

	req := &ChatCompletionRequest{
		Model: "o4-mini",
		Messages: []Message{
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
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
	if string(resp.Choices[0].Message.Content) != "\"Hello from Codex!\"" {
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		codexRespondWithJSON(w, r, map[string]any{
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		// Restore body for codexRespondWithJSON to read stream flag.
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
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
			{Role: "system", Content: json.RawMessage(`"You are a helpful assistant."`)},
			{Role: "user", Content: json.RawMessage(`"Hello"`)},
			{Role: "assistant", Content: json.RawMessage(`"Hi there!"`)},
			{Role: "user", Content: json.RawMessage(`"Tell me more"`)},
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

// --- VAL-CODEX-003: Sampling params are stripped for reasoning models ---

func TestCodexBackend_TemperatureAndMaxTokens(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedBody map[string]json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)
		// Restore body for codexRespondWithJSON to read stream flag.
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
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
		Model:       "gpt-5.4-mini",
		Messages:    []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		Temperature: &temp,
		MaxTokens:   &maxTokens,
		RawBody:     []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"test"}],"temperature":0.7,"top_p":0.9}`),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if _, ok := receivedBody["temperature"]; ok {
		t.Error("temperature should NOT be forwarded for GPT-5 reasoning models")
	}
	if _, ok := receivedBody["top_p"]; ok {
		t.Error("top_p should NOT be forwarded for GPT-5 reasoning models")
	}

	// max_output_tokens is NOT forwarded — the Codex API does not support it.
	if _, ok := receivedBody["max_output_tokens"]; ok {
		t.Error("max_output_tokens should NOT be forwarded to Codex API")
	}
}

func TestCodexBackend_GPT51NoneReasoningPreservesSamplingParams(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedBody map[string]json.RawMessage
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &receivedBody)
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
			"id":         "resp-test-gpt51",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "gpt-5.1-codex",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-gpt51",
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
	req := &ChatCompletionRequest{
		Model:       "gpt-5.1-codex",
		Messages:    []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		Temperature: &temp,
		RawBody:     []byte(`{"model":"gpt-5.1-codex","messages":[{"role":"user","content":"test"}],"temperature":0.7,"top_p":0.9,"reasoning":{"effort":"none"}}`),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if _, ok := receivedBody["temperature"]; !ok {
		t.Error("temperature should be forwarded when gpt-5.1 reasoning effort is none")
	}
	if _, ok := receivedBody["top_p"]; !ok {
		t.Error("top_p should be forwarded when gpt-5.1 reasoning effort is none")
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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

	for _, expected := range []config.ModelConfig{{ID: "o4-mini"}, {ID: "gpt-5.2-codex"}} {
		if !modelIDs[expected.ID] {
			t.Errorf("expected model %s in list", expected.ID)
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
	b := NewCodexBackend(cfg, nil, ts, nil, 0)

	models, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("expected default models when none configured")
	}

	if len(models) != len(defaultCodexModels) {
		t.Fatalf("default models count = %d, want %d", len(models), len(defaultCodexModels))
	}

	for i, want := range defaultCodexModels {
		if models[i].ID != want {
			t.Fatalf("default model[%d] = %q, want %q", i, models[i].ID, want)
		}
		if models[i].DisplayName == "" {
			t.Errorf("default model %q missing display_name", models[i].ID)
		}
		if models[i].ContextLength == nil || models[i].MaxOutputTokens == nil {
			t.Errorf("default model %q missing metadata", models[i].ID)
		}
	}
}

// --- SupportsModel tests ---

func TestCodexBackend_SupportsModel(t *testing.T) {
	tests := []struct {
		name   string
		models []config.ModelConfig
		check  string
		want   bool
	}{
		{"empty models list (accepts all)", nil, "anything", true},
		{"exact match", []config.ModelConfig{{ID: "o4-mini"}}, "o4-mini", true},
		{"no match", []config.ModelConfig{{ID: "o4-mini"}}, "gpt-5", false},
		{"wildcard match", []config.ModelConfig{{ID: "gpt-5/*"}}, "gpt-5/codex", true},
		{"wildcard no match", []config.ModelConfig{{ID: "gpt-5/*"}}, "o3/mini", false},
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
			b := NewCodexBackend(cfg, nil, ts, nil, 0)

			if got := b.SupportsModel(tt.check); got != tt.want {
				t.Errorf("SupportsModel(%q) = %v, want %v", tt.check, got, tt.want)
			}
		})
	}
}

// --- Force Streaming Tests ---

// TestCodexBackend_ForceStreaming_NonStreamingSendsStreamTrue verifies that ChatCompletion
// forces stream=true in the request to Codex for prompt caching consistency.
func TestCodexBackend_ForceStreaming_NonStreamingSendsStreamTrue(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	var receivedStream bool
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check that the request has stream=true.
		var reqBody map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &reqBody)
		if streamRaw, ok := reqBody["stream"]; ok {
			json.Unmarshal(streamRaw, &receivedStream)
		}
		// Restore body for codexRespondWithJSON.
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
			"id":         "resp-test-force-stream",
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "completed",
			"model":      "o4-mini",
			"output": []map[string]any{
				{
					"type":   "message",
					"id":     "msg-test-force-stream",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]any{
						{"type": "output_text", "text": "Force streaming response"},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		// Note: Stream is NOT set (client requests non-streaming).
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify the upstream received stream=true.
	if !receivedStream {
		t.Error("expected stream=true to be sent to upstream Codex even for non-streaming ChatCompletion requests")
	}

	// Verify the client received a normal non-streaming response.
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
	}
	if len(resp.Choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	if string(resp.Choices[0].Message.Content) != "\"Force streaming response\"" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Force streaming response")
	}
}

// TestCodexBackend_ForceStreaming_ResponseFromCompletedEvent verifies that the
// response is correctly built from the response.completed SSE event, including
// usage stats and finish_reason.
func TestCodexBackend_ForceStreaming_ResponseFromCompletedEvent(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		// Send text delta events (as Codex would).
		events := []string{
			`{"type":"response.output_text.delta","delta":"Hello ","item_id":"msg-1","output_index":0,"content_index":0}`,
			`{"type":"response.output_text.delta","delta":"World!","item_id":"msg-1","output_index":0,"content_index":0}`,
		}
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}

		// Send response.completed with full response data.
		completedEvent := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":         "resp-stream-to-nonstream",
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "completed",
				"model":      "o4-mini",
				"output": []map[string]any{
					{
						"type":   "message",
						"id":     "msg-1",
						"role":   "assistant",
						"status": "completed",
						"content": []map[string]any{
							{"type": "output_text", "text": "Hello World!"},
						},
					},
				},
				"usage": map[string]any{
					"input_tokens":  12,
					"output_tokens": 4,
					"total_tokens":  16,
				},
			},
		}
		completedJSON, _ := json.Marshal(completedEvent)
		fmt.Fprintf(w, "data: %s\n\n", completedJSON)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify the response was built from the completed event.
	if resp.ID != "resp-stream-to-nonstream" {
		t.Errorf("id = %q, want %q", resp.ID, "resp-stream-to-nonstream")
	}
	if string(resp.Choices[0].Message.Content) != "\"Hello World!\"" {
		t.Errorf("content = %q, want %q", resp.Choices[0].Message.Content, "Hello World!")
	}
	if resp.Usage == nil {
		t.Fatal("usage should be populated from completed event")
	}
	if resp.Usage.PromptTokens != 12 {
		t.Errorf("prompt_tokens = %d, want 12", resp.Usage.PromptTokens)
	}
	if resp.Usage.CompletionTokens != 4 {
		t.Errorf("completion_tokens = %d, want 4", resp.Usage.CompletionTokens)
	}
	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want %q", resp.Choices[0].FinishReason, "stop")
	}
}

// TestCodexBackend_ForceStreaming_StreamingUnchanged verifies that streaming
// ChatCompletion requests are NOT affected by the force-streaming change.
func TestCodexBackend_ForceStreaming_StreamingUnchanged(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify stream=true was sent.
		var reqBody map[string]json.RawMessage
		raw, _ := io.ReadAll(r.Body)
		json.Unmarshal(raw, &reqBody)
		var stream bool
		if streamRaw, ok := reqBody["stream"]; ok {
			json.Unmarshal(streamRaw, &stream)
		}
		if !stream {
			t.Error("streaming request should have stream=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.output_text.delta","delta":"Stream response","item_id":"msg-1","output_index":0,"content_index":0}`,
			`{"type":"response.completed","response":{"id":"resp-stream-unchanged","status":"completed","model":"o4-mini","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Stream response"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		}
		for _, event := range events {
			fmt.Fprintf(w, "data: %s\n\n", event)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		t.Error("expected [DONE] sentinel")
	}
	if !strings.Contains(bodyStr, "Stream response") {
		t.Error("expected 'Stream response' content in stream")
	}
}

// TestCodexBackend_ForceStreaming_IncompleteStatusFromSSE verifies that
// incomplete status from the response.completed SSE event translates to
// finish_reason "length" in the non-streaming response.
func TestCodexBackend_ForceStreaming_IncompleteStatusFromSSE(t *testing.T) {
	b, upstream, cleanup := codexTestHelper(t)
	defer cleanup()

	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")

		completedEvent := map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"id":         "resp-incomplete-sse",
				"object":     "response",
				"created_at": time.Now().Unix(),
				"status":     "incomplete",
				"model":      "o4-mini",
				"output": []map[string]any{
					{
						"type":   "message",
						"id":     "msg-1",
						"role":   "assistant",
						"status": "completed",
						"content": []map[string]any{
							{"type": "output_text", "text": "Truncated..."},
						},
					},
				},
				"usage": map[string]any{
					"input_tokens":  15,
					"output_tokens": 5,
					"total_tokens":  20,
				},
			},
		}
		completedJSON, _ := json.Marshal(completedEvent)
		fmt.Fprintf(w, "data: %s\n\n", completedJSON)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %v, want %q", resp.Choices[0].FinishReason, "length")
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
	b := NewCodexBackend(cfg, oauthHandler, ts, nil, 0)

	req := &ChatCompletionRequest{
		Model:    "o4-mini",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		codexRespondWithJSON(w, r, map[string]any{
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"Use tool X"`)}},
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
		// Restore body for codexRespondWithJSON to read stream flag.
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
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
		Model:    "gpt-5.1-codex",
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
		RawBody:  []byte(`{"model":"gpt-5.1-codex","messages":[{"role":"user","content":"test"}],"top_p":0.9,"presence_penalty":0.5,"frequency_penalty":0.3,"reasoning":{"effort":"none"}}`),
	}

	_, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify that ChatCompletion-only fields are excluded from the Codex request
	// (they are not part of the Responses API).
	if _, ok := receivedBody["presence_penalty"]; ok {
		t.Error("presence_penalty should NOT be forwarded to Codex Responses API")
	}
	if _, ok := receivedBody["frequency_penalty"]; ok {
		t.Error("frequency_penalty should NOT be forwarded to Codex Responses API")
	}
	// top_p should be preserved when sampling is supported.
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
		codexRespondWithJSON(w, r, map[string]any{
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
			Messages: []Message{{Role: "user", Content: json.RawMessage(`"non-stream test"`)}},
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
			Messages: []Message{{Role: "user", Content: json.RawMessage(`"stream test"`)}},
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
		Models:  []config.ModelConfig{{ID: "o4-mini"}},
	}
	b := NewCodexBackend(cfg, nil, ts, nil, 0)
	r.Register("codex", b)

	// Also register an OpenAI backend.
	openaiCfg := config.BackendConfig{
		Name:    "openrouter",
		Type:    "openai",
		BaseURL: "https://openrouter.ai/api/v1",
		APIKey:  "test-key",
		Models:  []config.ModelConfig{{ID: "openai/gpt-4o"}},
	}
	r.Register("openrouter", NewOpenAI(openaiCfg, 0))

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
		Models:  []config.ModelConfig{{ID: "o4-mini"}, {ID: "gpt-5.2-codex"}},
	}
	b := NewCodexBackend(cfg, nil, ts, nil, 0)
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
		Models:  []config.ModelConfig{{ID: "o4-mini"}},
	}
	b := NewCodexBackend(cfg, nil, ts, nil, 0)
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
		// Restore body for codexRespondWithJSON to read stream flag.
		r.Body = io.NopCloser(bytes.NewReader(raw))

		codexRespondWithJSON(w, r, map[string]any{
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
		Messages: []Message{{Role: "user", Content: func() json.RawMessage { b, _ := json.Marshal(unicodeContent); return b }()}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	// Verify unicode in response was preserved.
	if !strings.Contains(string(resp.Choices[0].Message.Content), "你好") {
		t.Error("Chinese characters not preserved in response")
	}
	if !strings.Contains(string(resp.Choices[0].Message.Content), "🎉") {
		t.Error("Emoji not preserved in response")
	}
	if !strings.Contains(string(resp.Choices[0].Message.Content), "café") {
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
		codexRespondWithJSON(w, r, map[string]any{
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
	messages[0] = Message{Role: "system", Content: json.RawMessage(`"You are helpful."`)}
	for i := 1; i < 51; i++ {
		if i%2 == 1 {
			b, _ := json.Marshal(fmt.Sprintf("User message %d", i))
			messages[i] = Message{Role: "user", Content: b}
		} else {
			b, _ := json.Marshal(fmt.Sprintf("Assistant reply %d", i))
			messages[i] = Message{Role: "assistant", Content: b}
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
		codexRespondWithJSON(w, r, map[string]any{
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
	}

	resp, err := b.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion after 401 retry: %v", err)
	}

	if string(resp.Choices[0].Message.Content) != "\"Success after retry\"" {
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
	b := NewCodexBackend(cfg, nil, ts, nil, 0)

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
		codexRespondWithJSON(w, r, map[string]any{
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
		Messages: []Message{{Role: "user", Content: json.RawMessage(`"test"`)}},
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
	// NeedsReauth is false when no token exists at all (it's 'not connected', not 'needs re-auth').
	// NeedsReauth is true only when a token exists but is expired and can't be refreshed.
	if status.NeedsReauth {
		t.Error("should NOT need re-auth when never connected; just not authenticated")
	}
	if status.TokenState != "missing" {
		t.Errorf("TokenState = %q, want %q", status.TokenState, "missing")
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

// --- Codex Device Code Login Tests ---

func TestCodexBackend_DeviceCodeLogin(t *testing.T) {
	dir, err := os.MkdirTemp("", "codex-device-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := oauth.NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Mock token server (for polling). Must stay alive for background goroutine.
	pollCount := 0
	tokenServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 2 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "device-code-access-token",
			"refresh_token": "device-code-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	// Mock device code server.
	deviceCodeServer := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oauth.CodexDeviceCodeResponse{
			DeviceCode:      "DC-codex-test",
			UserCode:        "CODE-1234",
			VerificationURI: "https://auth.openai.com/device",
			ExpiresIn:       900,
			Interval:        1,
		})
	}))
	defer deviceCodeServer.Close()

	oauthCfg := oauth.DefaultCodexOAuthConfig()
	oauthCfg.TokenURL = tokenServer.URL

	deviceCodeHandler := oauth.NewCodexDeviceCodeHandler(ts, oauthCfg,
		oauth.WithCodexDeviceCodeURL(deviceCodeServer.URL),
	)

	cfg := config.BackendConfig{
		Name:   "codex",
		Type:   "codex",
		Models: []config.ModelConfig{{ID: "o4-mini"}},
	}

	oauthHandler := oauth.NewCodexOAuthHandler(ts, oauthCfg)
	b := NewCodexBackend(cfg, oauthHandler, ts, deviceCodeHandler, 0)

	// Verify device code flow is supported.
	if !b.SupportsDeviceCodeFlow() {
		t.Error("SupportsDeviceCodeFlow should return true")
	}

	// Initiate device code login.
	authURL, state, err := b.InitiateDeviceCodeLogin()
	if err != nil {
		t.Fatalf("InitiateDeviceCodeLogin: %v", err)
	}

	if state == "" {
		t.Error("state should not be empty")
	}

	// Parse the returned JSON.
	var info DeviceCodeLoginInfo
	if err := json.Unmarshal([]byte(authURL), &info); err != nil {
		t.Fatalf("parsing device code info: %v", err)
	}

	if info.UserCode != "CODE-1234" {
		t.Errorf("UserCode = %q, want %q", info.UserCode, "CODE-1234")
	}
	if info.VerificationURI != "https://auth.openai.com/device" {
		t.Errorf("VerificationURI = %q, unexpected", info.VerificationURI)
	}

	// Wait for the background polling to complete.
	// The poll interval is 1 second, and it takes 2 pending + 1 success = 3 polls = ~3 seconds.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		token := ts.Get()
		if token != nil && token.AccessToken == "device-code-access-token" {
			// Token was stored successfully.
			if token.Source != "codex_device_code" {
				t.Errorf("Source = %q, want %q", token.Source, "codex_device_code")
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("timed out waiting for device code token to be stored")
}

func TestCodexBackend_DeviceCodeLogin_NoHandler(t *testing.T) {
	dir, err := os.MkdirTemp("", "codex-device-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := oauth.NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.BackendConfig{
		Name:   "codex",
		Type:   "codex",
		Models: []config.ModelConfig{{ID: "o4-mini"}},
	}

	// Create backend without device code handler (nil).
	b := NewCodexBackend(cfg, nil, ts, nil, 0)

	if b.SupportsDeviceCodeFlow() {
		t.Error("SupportsDeviceCodeFlow should return false when no handler")
	}

	_, _, err = b.InitiateDeviceCodeLogin()
	if err == nil {
		t.Fatal("expected error when no device code handler")
	}
}

func TestCodexBackend_DeviceCodeLogin_SupportsInterface(t *testing.T) {
	// Verify CodexBackend implements OAuthDeviceCodeLoginHandler.
	dir, err := os.MkdirTemp("", "codex-device-iface-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := oauth.NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.BackendConfig{Name: "codex", Type: "codex"}
	oauthHandler := oauth.NewCodexOAuthHandler(ts, nil)
	b := NewCodexBackend(cfg, oauthHandler, ts, nil, 0)

	// CodexBackend should implement the OAuthDeviceCodeLoginHandler interface.
	var _ OAuthDeviceCodeLoginHandler = b
}
