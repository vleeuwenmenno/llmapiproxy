package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/stats"
)

type Handler struct {
	registry  *backend.Registry
	collector *stats.Collector
}

func NewHandler(registry *backend.Registry, collector *stats.Collector) *Handler {
	return &Handler{
		registry:  registry,
		collector: collector,
	}
}

func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
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

	var req backend.ChatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.RawBody = body

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model field is required")
		return
	}

	b, modelID, err := h.registry.Resolve(req.Model)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	originalModel := req.Model
	req.Model = modelID

	start := time.Now()

	if req.Stream {
		h.handleStream(r.Context(), w, b, &req, originalModel, start)
	} else {
		h.handleNonStream(r.Context(), w, b, &req, originalModel, start)
	}
}

func (h *Handler) handleNonStream(ctx context.Context, w http.ResponseWriter, b backend.Backend, req *backend.ChatCompletionRequest, originalModel string, start time.Time) {
	resp, err := b.ChatCompletion(ctx, req)
	latency := time.Since(start).Milliseconds()

	rec := stats.Record{
		Timestamp: start,
		Backend:   b.Name(),
		Model:     originalModel,
		LatencyMs: latency,
		Stream:    false,
	}

	if err != nil {
		rec.StatusCode = http.StatusBadGateway
		rec.Error = err.Error()
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}

	if resp.Usage != nil {
		rec.PromptTokens = resp.Usage.PromptTokens
		rec.CompletionTokens = resp.Usage.CompletionTokens
		rec.TotalTokens = resp.Usage.TotalTokens
	}
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)

	resp.Model = originalModel

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) handleStream(ctx context.Context, w http.ResponseWriter, b backend.Backend, req *backend.ChatCompletionRequest, originalModel string, start time.Time) {
	stream, err := b.ChatCompletionStream(ctx, req)
	if err != nil {
		rec := stats.Record{
			Timestamp:  start,
			Backend:    b.Name(),
			Model:      originalModel,
			LatencyMs:  time.Since(start).Milliseconds(),
			StatusCode: http.StatusBadGateway,
			Error:      err.Error(),
			Stream:     true,
		}
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
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
		Timestamp: start,
		Backend:   b.Name(),
		Model:     originalModel,
		Stream:    true,
	}

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if ctx.Err() != nil {
			break
		}

		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			data := line[6:]
			rewritten, usage := rewriteStreamChunk(data, originalModel)
			if usage != nil {
				rec.PromptTokens = usage.PromptTokens
				rec.CompletionTokens = usage.CompletionTokens
				rec.TotalTokens = usage.TotalTokens
			}
			fmt.Fprintf(w, "data: %s\n\n", rewritten)
		} else if line != "" {
			fmt.Fprintf(w, "%s\n", line)
			if line == "data: [DONE]" {
				fmt.Fprint(w, "\n")
			}
		} else {
			continue
		}

		flusher.Flush()
	}

	if err := scanner.Err(); err != nil {
		log.Printf("stream scan error: %v", err)
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
}

func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var allModels []backend.Model
	for _, b := range h.registry.All() {
		models, err := b.ListModels(r.Context())
		if err != nil {
			log.Printf("error listing models from %s: %v", b.Name(), err)
			continue
		}
		for i := range models {
			models[i].ID = b.Name() + "/" + models[i].ID
		}
		allModels = append(allModels, models...)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backend.ModelList{
		Object: "list",
		Data:   allModels,
	})
}

func rewriteStreamChunk(data string, originalModel string) (string, *backend.Usage) {
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return data, nil
	}

	modelBytes, _ := json.Marshal(originalModel)
	chunk["model"] = modelBytes

	var usage *backend.Usage
	if u, ok := chunk["usage"]; ok {
		usage = &backend.Usage{}
		json.Unmarshal(u, usage)
	}

	out, err := json.Marshal(chunk)
	if err != nil {
		return data, usage
	}
	return string(out), usage
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "proxy_error",
		},
	})
}
