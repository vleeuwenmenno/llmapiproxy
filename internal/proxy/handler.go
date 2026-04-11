package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
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

	entries, strategy, staggerDelayMs, err := h.registry.ResolveRoute(req.Model, h.cfgMgr.Get().Routing)
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

	switch strategy {
	case config.StrategyRace:
		if req.Stream {
			h.handleRaceStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl)
		} else {
			h.handleRaceNonStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl)
		}
	case config.StrategyStaggeredRace:
		delay := time.Duration(staggerDelayMs) * time.Millisecond
		if delay <= 0 {
			delay = 500 * time.Millisecond
		}
		if req.Stream {
			h.handleStaggeredRaceStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl, delay)
		} else {
			h.handleStaggeredRaceNonStream(r.Context(), w, entries, &req, originalModel, start, clientName, cl, delay)
		}
	default: // priority and round-robin both use the ordered fallback loop
		if req.Stream {
			h.handleStream(r.Context(), w, entries, strategy, &req, originalModel, start, clientName, cl)
		} else {
			h.handleNonStream(r.Context(), w, entries, strategy, &req, originalModel, start, clientName, cl)
		}
	}
}

// attemptedBackends returns a comma-separated list of backend names from the entries slice.
func attemptedBackends(entries []backend.RouteEntry) string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Backend.Name()
	}
	return strings.Join(names, ",")
}

// triedBackends returns a comma-separated list of the first n backend names — i.e. those actually attempted.
func triedBackends(entries []backend.RouteEntry, n int) string {
	if n > len(entries) {
		n = len(entries)
	}
	names := make([]string, n)
	for i := 0; i < n; i++ {
		names[i] = entries[i].Backend.Name()
	}
	return strings.Join(names, ",")
}

func (h *Handler) handleNonStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, strategy string, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
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
				// 4xx errors (except 400 bad-request and 429 rate-limit) are client errors — don't retry.
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusBadRequest && be.StatusCode != http.StatusTooManyRequests {
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
		if resp.Usage != nil {
			rec.PromptTokens = resp.Usage.PromptTokens
			rec.CompletionTokens = resp.Usage.CompletionTokens
			rec.TotalTokens = resp.Usage.TotalTokens
			if resp.Usage.PromptTokensDetails != nil {
				rec.CachedTokens = resp.Usage.PromptTokensDetails.CachedTokens
			}
			if resp.Usage.CompletionTokensDetails != nil {
				rec.ReasoningTokens = resp.Usage.CompletionTokensDetails.ReasoningTokens
			}
		}
		rec.StatusCode = http.StatusOK
		h.collector.Record(rec)

		// Use raw body passthrough — only rewrite the model field, preserve all other fields.
		w.Header().Set("Content-Type", "application/json")
		if len(resp.RawBody) > 0 {
			w.Write(backend.RewriteResponseBody(resp.RawBody, originalModel))
		} else {
			resp.Model = originalModel
			json.NewEncoder(w).Encode(resp)
		}
		return
	}

	latency := time.Since(start).Milliseconds()
	rec := stats.Record{
		Timestamp:         start,
		Backend:           lastBackend,
		Model:             originalModel,
		LatencyMs:         latency,
		StatusCode:        http.StatusBadGateway,
		Error:             lastErr.Error(),
		Stream:            false,
		Client:            clientName,
		Strategy:          strategy,
		AttemptedBackends: triedBackends(entries, triedCount),
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
}

func (h *Handler) handleStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, strategy string, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var stream io.ReadCloser
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string
	triedCount := 0
	winnerIdx := 0

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
				// 4xx errors (except 400 bad-request and 429 rate-limit) are client errors — don't retry.
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusBadRequest && be.StatusCode != http.StatusTooManyRequests {
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
		winnerIdx = i
		break
	}

	if stream == nil {
		rec := stats.Record{
			Timestamp:         start,
			Backend:           lastBackend,
			Model:             originalModel,
			LatencyMs:         time.Since(start).Milliseconds(),
			StatusCode:        http.StatusBadGateway,
			Error:             lastErr.Error(),
			Stream:            true,
			Client:            clientName,
			Strategy:          strategy,
			AttemptedBackends: triedBackends(entries, triedCount),
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
		Fallback:          winnerIdx > 0,
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
			applyUsageToRecord(&rec, usage)
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

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("stream scan error: %v", err)
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
}

// raceResult holds the outcome of a single backend attempt in race mode.
type raceResult struct {
	resp  *backend.ChatCompletionResponse
	be    backend.Backend
	err   error
	beErr *backend.BackendError
}

func (h *Handler) handleRaceNonStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	attempted := attemptedBackends(entries)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resultCh := make(chan raceResult, len(entries))
	var wg sync.WaitGroup
	for _, entry := range entries {
		wg.Add(1)
		e := entry
		go func() {
			defer wg.Done()
			reqCopy := *req
			reqCopy.Model = e.ModelID
			if cl != nil && cl.BackendKeys != nil {
				if k, ok := cl.BackendKeys[e.Backend.Name()]; ok {
					reqCopy.APIKeyOverride = k
				}
			}
			resp, err := e.Backend.ChatCompletion(ctx, &reqCopy)
			rr := raceResult{resp: resp, be: e.Backend, err: err}
			errors.As(err, &rr.beErr)
			resultCh <- rr
		}()
	}
	// Close channel once all goroutines finish so we can drain below.
	go func() { wg.Wait(); close(resultCh) }()

	var lastErr error
	var lastBE *backend.BackendError
	var lastBEName string
	received := 0
	for rr := range resultCh {
		received++
		if rr.err == nil {
			// Winner — cancel all remaining, return response.
			cancel()
			latency := time.Since(start).Milliseconds()
			rec := stats.Record{
				Timestamp:         start,
				Backend:           rr.be.Name(),
				Model:             originalModel,
				LatencyMs:         latency,
				StatusCode:        http.StatusOK,
				Stream:            false,
				Client:            clientName,
				Strategy:          config.StrategyRace,
				AttemptedBackends: attempted,
			}
			if rr.resp.Usage != nil {
				rec.PromptTokens = rr.resp.Usage.PromptTokens
				rec.CompletionTokens = rr.resp.Usage.CompletionTokens
				rec.TotalTokens = rr.resp.Usage.TotalTokens
				if rr.resp.Usage.PromptTokensDetails != nil {
					rec.CachedTokens = rr.resp.Usage.PromptTokensDetails.CachedTokens
				}
				if rr.resp.Usage.CompletionTokensDetails != nil {
					rec.ReasoningTokens = rr.resp.Usage.CompletionTokensDetails.ReasoningTokens
				}
			}
			h.collector.Record(rec)
			w.Header().Set("Content-Type", "application/json")
			if len(rr.resp.RawBody) > 0 {
				w.Write(backend.RewriteResponseBody(rr.resp.RawBody, originalModel))
			} else {
				rr.resp.Model = originalModel
				json.NewEncoder(w).Encode(rr.resp)
			}
			return
		}
		lastErr = rr.err
		lastBEName = rr.be.Name()
		if rr.beErr != nil {
			lastBE = rr.beErr
		}
		if received == len(entries) {
			break
		}
	}

	// All backends failed.
	latency := time.Since(start).Milliseconds()
	rec := stats.Record{
		Timestamp:         start,
		Backend:           lastBEName,
		Model:             originalModel,
		LatencyMs:         latency,
		StatusCode:        http.StatusBadGateway,
		Error:             lastErr.Error(),
		Stream:            false,
		Client:            clientName,
		Strategy:          config.StrategyRace,
		AttemptedBackends: attempted,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
}

// raceStreamResult carries the winning stream and its initial buffered data.
type raceStreamResult struct {
	stream     io.ReadCloser
	buffered   []byte
	be         backend.Backend
	cancelOurs context.CancelFunc
}

func (h *Handler) handleRaceStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	attempted := attemptedBackends(entries)
	parentCtx, parentCancel := context.WithCancel(ctx)
	defer parentCancel()

	type streamAttempt struct {
		result raceStreamResult
		err    error
	}

	resultCh := make(chan streamAttempt, len(entries))
	// Per-backend cancels so we can cancel losers individually.
	cancels := make([]context.CancelFunc, len(entries))

	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		e := entry
		bCtx, bCancel := context.WithCancel(parentCtx)
		cancels[i] = bCancel
		go func() {
			defer wg.Done()
			reqCopy := *req
			reqCopy.Model = e.ModelID
			if cl != nil && cl.BackendKeys != nil {
				if k, ok := cl.BackendKeys[e.Backend.Name()]; ok {
					reqCopy.APIKeyOverride = k
				}
			}
			stream, err := e.Backend.ChatCompletionStream(bCtx, &reqCopy)
			if err != nil {
				resultCh <- streamAttempt{err: err}
				return
			}
			// Buffer initial data to confirm the stream is alive before declaring a winner.
			buf := make([]byte, 4096)
			n, readErr := stream.Read(buf)
			if readErr != nil && n == 0 {
				stream.Close()
				resultCh <- streamAttempt{err: fmt.Errorf("stream read failed: %w", readErr)}
				return
			}
			resultCh <- streamAttempt{result: raceStreamResult{
				stream:   stream,
				buffered: buf[:n],
				be:       e.Backend,
			}}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var winner *raceStreamResult
	var lastErr error
	received := 0
	for attempt := range resultCh {
		received++
		if attempt.err == nil && winner == nil {
			// First successful stream — cancel all other backend contexts.
			parentCancel()
			w := attempt.result
			winner = &w
			if received == len(entries) {
				break
			}
			// Keep draining so goroutines can finish.
			continue
		}
		if attempt.err != nil && winner == nil {
			lastErr = attempt.err
		}
		if received == len(entries) {
			break
		}
	}

	if winner == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("all backends failed to stream")
		}
		rec := stats.Record{
			Timestamp:         start,
			Backend:           "",
			Model:             originalModel,
			LatencyMs:         time.Since(start).Milliseconds(),
			StatusCode:        http.StatusBadGateway,
			Error:             lastErr.Error(),
			Stream:            true,
			Client:            clientName,
			Strategy:          config.StrategyRace,
			AttemptedBackends: attempted,
		}
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
		return
	}
	defer winner.stream.Close()

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
		Backend:           winner.be.Name(),
		Model:             originalModel,
		Stream:            true,
		Client:            clientName,
		Strategy:          config.StrategyRace,
		AttemptedBackends: attempted,
	}

	// Replay buffered data first, then stream the rest.
	fullStream := io.MultiReader(bytes.NewReader(winner.buffered), winner.stream)
	scanner := bufio.NewScanner(fullStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if ctx.Err() != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			data := line[6:]
			rewritten, usage := rewriteStreamChunk(data, originalModel)
			applyUsageToRecord(&rec, usage)
			fmt.Fprintf(w, "data: %s\n\n", rewritten)
		} else if line != "" {
			fmt.Fprintf(w, "%s\n", line)
			if line == "data: [DONE]" {
				fmt.Fprint(w, "\n")
			}
		} else {
			fmt.Fprint(w, "\n")
		}
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("race stream scan error: %v", err)
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
}

// handleStaggeredRaceNonStream fires backends in priority order with `delay` between each launch.
// The first successful response wins; remaining in-flight requests are cancelled.
func (h *Handler) handleStaggeredRaceNonStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig, delay time.Duration) {
	attempted := attemptedBackends(entries)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	resultCh := make(chan raceResult, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		e := entry
		sleepDur := time.Duration(i) * delay
		go func() {
			defer wg.Done()
			if sleepDur > 0 {
				timer := time.NewTimer(sleepDur)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					resultCh <- raceResult{err: ctx.Err(), be: e.Backend}
					return
				}
			}
			reqCopy := *req
			reqCopy.Model = e.ModelID
			if cl != nil && cl.BackendKeys != nil {
				if k, ok := cl.BackendKeys[e.Backend.Name()]; ok {
					reqCopy.APIKeyOverride = k
				}
			}
			resp, err := e.Backend.ChatCompletion(ctx, &reqCopy)
			rr := raceResult{resp: resp, be: e.Backend, err: err}
			errors.As(err, &rr.beErr)
			resultCh <- rr
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var lastErr error
	var lastBE *backend.BackendError
	var lastBEName string
	received := 0
	for rr := range resultCh {
		received++
		if rr.err == nil {
			cancel()
			latency := time.Since(start).Milliseconds()
			rec := stats.Record{
				Timestamp:         start,
				Backend:           rr.be.Name(),
				Model:             originalModel,
				LatencyMs:         latency,
				StatusCode:        http.StatusOK,
				Stream:            false,
				Client:            clientName,
				Strategy:          config.StrategyStaggeredRace,
				AttemptedBackends: attempted,
			}
			if rr.resp.Usage != nil {
				rec.PromptTokens = rr.resp.Usage.PromptTokens
				rec.CompletionTokens = rr.resp.Usage.CompletionTokens
				rec.TotalTokens = rr.resp.Usage.TotalTokens
				if rr.resp.Usage.PromptTokensDetails != nil {
					rec.CachedTokens = rr.resp.Usage.PromptTokensDetails.CachedTokens
				}
				if rr.resp.Usage.CompletionTokensDetails != nil {
					rec.ReasoningTokens = rr.resp.Usage.CompletionTokensDetails.ReasoningTokens
				}
			}
			h.collector.Record(rec)
			w.Header().Set("Content-Type", "application/json")
			if len(rr.resp.RawBody) > 0 {
				w.Write(backend.RewriteResponseBody(rr.resp.RawBody, originalModel))
			} else {
				rr.resp.Model = originalModel
				json.NewEncoder(w).Encode(rr.resp)
			}
			return
		}
		lastErr = rr.err
		lastBEName = rr.be.Name()
		if rr.beErr != nil {
			lastBE = rr.beErr
		}
		if received == len(entries) {
			break
		}
	}

	latency := time.Since(start).Milliseconds()
	errMsg := "all backends failed"
	if lastErr != nil {
		errMsg = lastErr.Error()
	}
	rec := stats.Record{
		Timestamp:         start,
		Backend:           lastBEName,
		Model:             originalModel,
		LatencyMs:         latency,
		StatusCode:        http.StatusBadGateway,
		Error:             errMsg,
		Stream:            false,
		Client:            clientName,
		Strategy:          config.StrategyStaggeredRace,
		AttemptedBackends: attempted,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+errMsg)
}

// handleStaggeredRaceStream is the streaming variant of handleStaggeredRaceNonStream.
func (h *Handler) handleStaggeredRaceStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig, delay time.Duration) {
	attempted := attemptedBackends(entries)
	parentCtx, parentCancel := context.WithCancel(ctx)
	defer parentCancel()

	type streamAttempt struct {
		result raceStreamResult
		err    error
	}

	resultCh := make(chan streamAttempt, len(entries))
	var wg sync.WaitGroup
	for i, entry := range entries {
		wg.Add(1)
		e := entry
		bCtx, bCancel := context.WithCancel(parentCtx)
		_ = bCancel // cancelled via parentCancel when a winner is found
		sleepDur := time.Duration(i) * delay
		go func() {
			defer wg.Done()
			if sleepDur > 0 {
				timer := time.NewTimer(sleepDur)
				select {
				case <-timer.C:
				case <-bCtx.Done():
					timer.Stop()
					resultCh <- streamAttempt{err: bCtx.Err()}
					return
				}
			}
			reqCopy := *req
			reqCopy.Model = e.ModelID
			if cl != nil && cl.BackendKeys != nil {
				if k, ok := cl.BackendKeys[e.Backend.Name()]; ok {
					reqCopy.APIKeyOverride = k
				}
			}
			stream, err := e.Backend.ChatCompletionStream(bCtx, &reqCopy)
			if err != nil {
				resultCh <- streamAttempt{err: err}
				return
			}
			buf := make([]byte, 4096)
			n, readErr := stream.Read(buf)
			if readErr != nil && n == 0 {
				stream.Close()
				resultCh <- streamAttempt{err: fmt.Errorf("stream read failed: %w", readErr)}
				return
			}
			resultCh <- streamAttempt{result: raceStreamResult{
				stream:   stream,
				buffered: buf[:n],
				be:       e.Backend,
			}}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var winner *raceStreamResult
	var lastErr error
	received := 0
	for attempt := range resultCh {
		received++
		if attempt.err == nil && winner == nil {
			parentCancel()
			res := attempt.result
			winner = &res
			if received == len(entries) {
				break
			}
			continue
		}
		if attempt.err != nil && winner == nil {
			lastErr = attempt.err
		}
		if received == len(entries) {
			break
		}
	}

	if winner == nil {
		if lastErr == nil {
			lastErr = fmt.Errorf("all backends failed to stream")
		}
		rec := stats.Record{
			Timestamp:         start,
			Backend:           "",
			Model:             originalModel,
			LatencyMs:         time.Since(start).Milliseconds(),
			StatusCode:        http.StatusBadGateway,
			Error:             lastErr.Error(),
			Stream:            true,
			Client:            clientName,
			Strategy:          config.StrategyStaggeredRace,
			AttemptedBackends: attempted,
		}
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
		return
	}
	defer winner.stream.Close()

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
		Backend:           winner.be.Name(),
		Model:             originalModel,
		Stream:            true,
		Client:            clientName,
		Strategy:          config.StrategyStaggeredRace,
		AttemptedBackends: attempted,
	}

	fullStream := io.MultiReader(bytes.NewReader(winner.buffered), winner.stream)
	scanner := bufio.NewScanner(fullStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if ctx.Err() != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
			data := line[6:]
			rewritten, usage := rewriteStreamChunk(data, originalModel)
			applyUsageToRecord(&rec, usage)
			fmt.Fprintf(w, "data: %s\n\n", rewritten)
		} else if line != "" {
			fmt.Fprintf(w, "%s\n", line)
			if line == "data: [DONE]" {
				fmt.Fprint(w, "\n")
			}
		} else {
			fmt.Fprint(w, "\n")
		}
		flusher.Flush()
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("staggered-race stream scan error: %v", err)
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

// applyUsageToRecord copies usage data (including cache/reasoning details) into a stats record.
func applyUsageToRecord(rec *stats.Record, usage *backend.Usage) {
	if usage == nil {
		return
	}
	rec.PromptTokens = usage.PromptTokens
	rec.CompletionTokens = usage.CompletionTokens
	rec.TotalTokens = usage.TotalTokens
	if usage.PromptTokensDetails != nil {
		rec.CachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil {
		rec.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
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
