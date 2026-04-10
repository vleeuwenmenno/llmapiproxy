package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/stats"
)

type Handler struct {
	registry  *backend.Registry
	collector *stats.Collector
	cfgMgr    *config.Manager
}

func NewHandler(registry *backend.Registry, collector *stats.Collector, cfgMgr *config.Manager) *Handler {
	return &Handler{
		registry:  registry,
		collector: collector,
		cfgMgr:    cfgMgr,
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

	entries, err := h.registry.ResolveRoute(req.Model, h.cfgMgr.Get().Routing)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	cl := ClientFromContext(r.Context())
	clientName := ""
	if cl != nil {
		clientName = cl.Name
	}

	originalModel := req.Model
	start := time.Now()

	if req.Stream {
		h.handleStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl)
	} else {
		h.handleNonStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl)
	}
}

func (h *Handler) handleNonStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string

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
			lastErr = err
			lastBackend = entry.Backend.Name()
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				if be.StatusCode >= 400 && be.StatusCode < 500 {
					break
				}
			}
			if i < len(entries)-1 {
				continue
			}
			break
		}

		latency := time.Since(start).Milliseconds()
		rec := stats.Record{
			Timestamp: start,
			Backend:   entry.Backend.Name(),
			Model:     originalModel,
			LatencyMs: latency,
			Stream:    false,
			Client:    clientName,
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
		return
	}

	latency := time.Since(start).Milliseconds()
	rec := stats.Record{
		Timestamp:  start,
		Backend:    lastBackend,
		Model:      originalModel,
		LatencyMs:  latency,
		StatusCode: http.StatusBadGateway,
		Error:      lastErr.Error(),
		Stream:     false,
		Client:     clientName,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
}

func (h *Handler) handleStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var stream io.ReadCloser
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string

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
			lastErr = err
			lastBackend = entry.Backend.Name()
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				if be.StatusCode >= 400 && be.StatusCode < 500 {
					break
				}
			}
			if i < len(entries)-1 {
				continue
			}
			break
		}
		lastBackend = entry.Backend.Name()
		break
	}

	if stream == nil {
		rec := stats.Record{
			Timestamp:  start,
			Backend:    lastBackend,
			Model:      originalModel,
			LatencyMs:  time.Since(start).Milliseconds(),
			StatusCode: http.StatusBadGateway,
			Error:      lastErr.Error(),
			Stream:     true,
			Client:     clientName,
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
		Timestamp: start,
		Backend:   lastBackend,
		Model:     originalModel,
		Stream:    true,
		Client:    clientName,
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

	// Collect models from all backends and deduplicate by model ID.
	// For duplicates, merge metadata: largest context/output windows, union of capabilities.
	seen := make(map[string]*backend.Model)
	var order []string

	for _, b := range h.registry.All() {
		models, err := b.ListModels(r.Context())
		if err != nil {
			log.Printf("error listing models from %s: %v", b.Name(), err)
			continue
		}
		for _, m := range models {
			if existing, ok := seen[m.ID]; ok {
				// Merge: keep largest context_length and max_output_tokens.
				if m.ContextLength != nil && (existing.ContextLength == nil || *m.ContextLength > *existing.ContextLength) {
					existing.ContextLength = m.ContextLength
				}
				if m.MaxOutputTokens != nil && (existing.MaxOutputTokens == nil || *m.MaxOutputTokens > *existing.MaxOutputTokens) {
					existing.MaxOutputTokens = m.MaxOutputTokens
				}
				// Union capabilities.
				capSet := make(map[string]bool, len(existing.Capabilities))
				for _, c := range existing.Capabilities {
					capSet[c] = true
				}
				for _, c := range m.Capabilities {
					if !capSet[c] {
						existing.Capabilities = append(existing.Capabilities, c)
					}
				}
			} else {
				mCopy := m
				mCopy.OwnedBy = "llmapiproxy"
				seen[m.ID] = &mCopy
				order = append(order, m.ID)
			}
		}
	}

	allModels := make([]backend.Model, 0, len(order))
	for _, id := range order {
		allModels = append(allModels, *seen[id])
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
