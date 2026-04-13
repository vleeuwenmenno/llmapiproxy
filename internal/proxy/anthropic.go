package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/stats"
)

type anthropicMessagesRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system,omitempty"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (h *Handler) AnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var req anthropicMessagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model field is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages field is required")
		return
	}
	if req.MaxTokens <= 0 {
		writeError(w, http.StatusBadRequest, "max_tokens must be > 0")
		return
	}

	chatReq, err := anthropicToChatCompletion(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	entries, strategy, _, err := h.registry.ResolveRoute(req.Model, h.cfgMgr.Get().Routing)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cl := ClientFromContext(r.Context())
	clientName := ""
	if cl != nil {
		clientName = cl.Name
	}

	start := time.Now()
	log.Info().Str("model", req.Model).Str("strategy", strategy).Str("client", clientName).Bool("stream", req.Stream).Msg("anthropic messages request")

	if req.Stream {
		h.handleAnthropicStream(r.Context(), w, entries, strategy, chatReq, req.Model, start, clientName, cl)
		return
	}
	h.handleAnthropicNonStream(r.Context(), w, entries, strategy, chatReq, req.Model, start, clientName, cl)
}

func anthropicToChatCompletion(req *anthropicMessagesRequest) (*backend.ChatCompletionRequest, error) {
	openAIMessages := make([]map[string]any, 0, len(req.Messages)+1)
	backendMessages := make([]backend.Message, 0, len(req.Messages)+1)

	if systemText := anthropicTextValue(req.System); strings.TrimSpace(systemText) != "" {
		openAIMessages = append(openAIMessages, map[string]any{
			"role":    "system",
			"content": systemText,
		})
		backendMessages = append(backendMessages, backend.Message{
			Role:    "system",
			Content: mustJSONRaw(systemText),
		})
	}

	for _, msg := range req.Messages {
		content := anthropicTextValue(msg.Content)
		openAIMessages = append(openAIMessages, map[string]any{
			"role":    msg.Role,
			"content": content,
		})
		backendMessages = append(backendMessages, backend.Message{
			Role:    msg.Role,
			Content: mustJSONRaw(content),
		})
	}

	raw := map[string]any{
		"model":      req.Model,
		"messages":   openAIMessages,
		"max_tokens": req.MaxTokens,
		"stream":     req.Stream,
	}
	if req.Temperature != nil {
		raw["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		raw["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		raw["stop"] = req.StopSequences
	}
	if req.Metadata != nil {
		if userID, ok := req.Metadata["user_id"].(string); ok && strings.TrimSpace(userID) != "" {
			raw["user"] = userID
		}
	}

	rawBody, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("failed to build translated request: %w", err)
	}

	maxTokens := req.MaxTokens
	return &backend.ChatCompletionRequest{
		Model:       req.Model,
		Messages:    backendMessages,
		Stream:      req.Stream,
		Temperature: req.Temperature,
		MaxTokens:   &maxTokens,
		RawBody:     rawBody,
	}, nil
}

func anthropicTextValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		t, _ := block["type"].(string)
		switch t {
		case "text":
			if text := anthropicLooseValue(block["text"]); text != "" {
				parts = append(parts, text)
			}
		case "tool_result":
			if content := anthropicLooseValue(block["content"]); content != "" {
				parts = append(parts, content)
			}
		case "output_text":
			if text := anthropicLooseValue(block["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}

	return strings.Join(parts, "\n")
}

func anthropicLooseValue(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case map[string]any:
		for _, key := range []string{"text", "value", "content"} {
			if nested, ok := value[key]; ok {
				if text := anthropicLooseValue(nested); text != "" {
					return text
				}
			}
		}
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			if text := anthropicLooseValue(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func mustJSONRaw(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func anthropicResponseFromChat(resp *backend.ChatCompletionResponse, originalModel string) ([]byte, error) {
	stopReason := "end_turn"
	text := ""

	if len(resp.Choices) > 0 {
		choice := resp.Choices[0]
		if choice.FinishReason != nil {
			stopReason = anthropicStopReason(*choice.FinishReason)
		}
		if choice.Message != nil {
			text = anthropicTextValue(choice.Message.Content)
		}
	}

	out := map[string]any{
		"id":            resp.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         originalModel,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"content": []map[string]any{
			{
				"type": "text",
				"text": text,
			},
		},
	}
	if resp.Usage != nil {
		out["usage"] = anthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}

	return json.Marshal(out)
}

func anthropicStopReason(finishReason string) string {
	switch finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func (h *Handler) handleAnthropicNonStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, strategy string, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string
	triedCount := 0

	for i, entry := range entries {
		reqCopy := *req
		reqCopy.Model = entry.ModelID
		if cl != nil && cl.BackendKeys != nil {
			if k, ok := cl.BackendKeys[entry.Backend.Name()]; ok {
				reqCopy.APIKeyOverride = k
			}
		}

		resp, err := entry.Backend.ChatCompletion(ctx, &reqCopy)
		if err != nil {
			triedCount++
			lastErr = err
			lastBackend = entry.Backend.Name()
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusTooManyRequests {
					break
				}
			}
			if i < len(entries)-1 {
				continue
			}
			break
		}

		latency := time.Since(start).Milliseconds()
		triedCount++
		rec := stats.Record{
			Timestamp:         start,
			Backend:           entry.Backend.Name(),
			Model:             originalModel,
			LatencyMs:         latency,
			Stream:            false,
			Client:            clientName,
			Strategy:          strategy,
			AttemptedBackends: triedBackends(entries, triedCount),
			Fallback:          i > 0,
		}
		applyUsageToRecord(&rec, resp.Usage)
		stats.ComputeTPS(&rec, time.Time{}, time.Time{})
		rec.StatusCode = http.StatusOK
		h.collector.Record(rec)

		out, err := anthropicResponseFromChat(resp, originalModel)
		if err != nil {
			writeError(w, http.StatusBadGateway, "anthropic translation error: "+err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
		log.Info().Str("model", originalModel).Str("backend", entry.Backend.Name()).Int64("latency_ms", latency).Float64("tps", rec.TPS).Bool("fallback", i > 0).Str("client", clientName).Msg("anthropic completion complete")
		return
	}

	latency := time.Since(start).Milliseconds()
	rec := stats.Record{
		Timestamp:         start,
		Backend:           lastBackend,
		Model:             originalModel,
		LatencyMs:         latency,
		StatusCode:        http.StatusBadGateway,
		Stream:            false,
		Client:            clientName,
		Strategy:          strategy,
		AttemptedBackends: triedBackends(entries, triedCount),
	}
	if lastErr != nil {
		rec.Error = lastErr.Error()
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
}

func (h *Handler) handleAnthropicStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, strategy string, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var stream io.ReadCloser
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string
	triedCount := 0

	for i, entry := range entries {
		reqCopy := *req
		reqCopy.Model = entry.ModelID
		if cl != nil && cl.BackendKeys != nil {
			if k, ok := cl.BackendKeys[entry.Backend.Name()]; ok {
				reqCopy.APIKeyOverride = k
			}
		}

		var err error
		stream, err = entry.Backend.ChatCompletionStream(ctx, &reqCopy)
		if err != nil {
			triedCount++
			lastErr = err
			lastBackend = entry.Backend.Name()
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusTooManyRequests {
					break
				}
			}
			if i < len(entries)-1 {
				continue
			}
			break
		}
		lastBackend = entry.Backend.Name()
		triedCount++
		break
	}

	if stream == nil {
		rec := stats.Record{
			Timestamp:         start,
			Backend:           lastBackend,
			Model:             originalModel,
			LatencyMs:         time.Since(start).Milliseconds(),
			StatusCode:        http.StatusBadGateway,
			Stream:            true,
			Client:            clientName,
			Strategy:          strategy,
			AttemptedBackends: triedBackends(entries, triedCount),
		}
		if lastErr != nil {
			rec.Error = lastErr.Error()
		}
		if lastBE != nil {
			rec.ResponseBody = lastBE.Body
		}
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
		return
	}
	defer stream.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rec := stats.Record{
		Timestamp:         start,
		Backend:           lastBackend,
		Model:             originalModel,
		Stream:            true,
		Client:            clientName,
		Strategy:          strategy,
		AttemptedBackends: triedBackends(entries, triedCount),
	}

	writeAnthropicSSE(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            fmt.Sprintf("msg_%d", start.UnixNano()),
			"type":          "message",
			"role":          "assistant",
			"model":         originalModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	})
	writeAnthropicSSE(w, flusher, "content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	stopReason := "end_turn"
	var finalUsage *backend.Usage
	var firstTokenAt time.Time
	for scanner.Scan() {
		line := scanner.Text()
		if ctx.Err() != nil {
			break
		}
		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *backend.Usage `json:"usage,omitempty"`
		}
		if err := json.Unmarshal([]byte(line[6:]), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			finalUsage = chunk.Usage
			applyUsageToRecord(&rec, chunk.Usage)
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if text := chunk.Choices[0].Delta.Content; text != "" {
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
			writeAnthropicSSE(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{
					"type": "text_delta",
					"text": text,
				},
			})
		}
		if chunk.Choices[0].FinishReason != nil {
			stopReason = anthropicStopReason(*chunk.Choices[0].FinishReason)
		}
	}

	writeAnthropicSSE(w, flusher, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})

	delta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
	}
	if finalUsage != nil {
		delta["usage"] = anthropicUsage{
			InputTokens:  finalUsage.PromptTokens,
			OutputTokens: finalUsage.CompletionTokens,
		}
	}
	writeAnthropicSSE(w, flusher, "message_delta", delta)
	writeAnthropicSSE(w, flusher, "message_stop", map[string]any{
		"type": "message_stop",
	})

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		log.Error().Err(err).Msg("anthropic stream scan error")
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	stats.ComputeTPS(&rec, firstTokenAt, time.Now())
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
}

func writeAnthropicSSE(w io.Writer, flusher http.Flusher, event string, payload any) {
	body, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", body)
	flusher.Flush()
}
