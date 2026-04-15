package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/stats"
)

// CircuitManager is the interface for the circuit breaker system.
// Defined here to avoid circular imports between proxy and circuit packages.
type CircuitManager interface {
	Record429(backendName string, retryAfterHint time.Duration)
	RecordSuccess(backendName string)
	IsOpen(backendName string) bool
	FilterEntries(names []string) []string
	Enabled() bool
}

type Handler struct {
	registry  *backend.Registry
	collector *stats.Collector
	cfgMgr    *config.Manager
	circuit   CircuitManager
}

func NewHandler(registry *backend.Registry, collector *stats.Collector, cfgMgr *config.Manager, circuit CircuitManager) *Handler {
	return &Handler{
		registry:  registry,
		collector: collector,
		cfgMgr:    cfgMgr,
		circuit:   circuit,
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

	// Filter out backends with tripped circuit breakers.
	entries = h.filterCircuitEntries(entries)

	cl := ClientFromContext(r.Context())
	clientName := ""
	if cl != nil {
		clientName = cl.Name
	}

	originalModel := req.Model
	start := time.Now()

	log.Info().Str("model", originalModel).Str("strategy", strategy).Str("client", clientName).Bool("stream", req.Stream).Msg("completion request")

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
	var attempts []stats.Attempt

	for i, entry := range entries {
		reqCopy := *req
		reqCopy.Model = entry.ModelID
		if cl != nil && cl.BackendKeys != nil {
			if k, ok := cl.BackendKeys[entry.Backend.Name()]; ok {
				reqCopy.APIKeyOverride = k
			}
		}

		attemptStart := time.Now()
		resp, err := entry.Backend.ChatCompletion(ctx, &reqCopy)
		if err != nil {
			triedCount++
			lastErr = err
			lastBackend = entry.Backend.Name()
			a := stats.Attempt{
				AttemptOrder: i,
				Backend:      entry.Backend.Name(),
				Error:        err.Error(),
				LatencyMs:    time.Since(attemptStart).Milliseconds(),
			}
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				a.StatusCode = be.StatusCode
				a.ResponseBody = be.Body
				// Record 429 for circuit breaker.
				if be.StatusCode == http.StatusTooManyRequests {
					h.circuitRecord429(entry.Backend.Name(), be)
				}
				// 4xx errors (except 429 rate-limit and 404 not-found) are client errors — don't retry.
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusTooManyRequests && be.StatusCode != http.StatusNotFound {
					attempts = append(attempts, a)
					break
				}
			}
			attempts = append(attempts, a)
			if i < len(entries)-1 {
				continue
			}
			break
		}

		latency := time.Since(start).Milliseconds()
		triedCount++
		h.circuitRecordSuccess(entry.Backend.Name())
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
			Attempts:          attempts,
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
		stats.ComputeTPS(&rec, time.Time{}, time.Now())
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
		log.Info().Str("model", originalModel).Str("backend", entry.Backend.Name()).Int64("latency_ms", latency).Float64("tps", rec.TPS).Bool("fallback", i > 0).Str("client", clientName).Msg("completion complete")
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
		Attempts:          attempts,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	rec.RequestBody = string(req.RawBody)
	h.collector.Record(rec)
	log.Error().Str("model", originalModel).Str("backend", lastBackend).Int64("latency_ms", latency).Bool("stream", false).Str("client", clientName).Msg("completion failed: all backends error")
	writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
}

func (h *Handler) handleStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, strategy string, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig) {
	var stream io.ReadCloser
	var lastErr error
	var lastBE *backend.BackendError
	var lastBackend string
	triedCount := 0
	winnerIdx := 0
	var attempts []stats.Attempt

	for i, entry := range entries {
		reqCopy := *req
		reqCopy.Model = entry.ModelID
		if cl != nil && cl.BackendKeys != nil {
			if k, ok := cl.BackendKeys[entry.Backend.Name()]; ok {
				reqCopy.APIKeyOverride = k
			}
		}

		attemptStart := time.Now()
		var err error
		stream, err = entry.Backend.ChatCompletionStream(ctx, &reqCopy)
		if err != nil {
			triedCount++
			lastErr = err
			lastBackend = entry.Backend.Name()
			a := stats.Attempt{
				AttemptOrder: i,
				Backend:      entry.Backend.Name(),
				Error:        err.Error(),
				LatencyMs:    time.Since(attemptStart).Milliseconds(),
			}
			var be *backend.BackendError
			if errors.As(err, &be) {
				lastBE = be
				a.StatusCode = be.StatusCode
				a.ResponseBody = be.Body
				// Record 429 for circuit breaker.
				if be.StatusCode == http.StatusTooManyRequests {
					h.circuitRecord429(entry.Backend.Name(), be)
				}
				// 4xx errors (except 429 rate-limit and 404 not-found) are client errors — don't retry.
				if be.StatusCode >= 400 && be.StatusCode < 500 && be.StatusCode != http.StatusTooManyRequests && be.StatusCode != http.StatusNotFound {
					attempts = append(attempts, a)
					break
				}
			}
			attempts = append(attempts, a)
			if i < len(entries)-1 {
				continue
			}
			break
		}
		lastBackend = entry.Backend.Name()
		triedCount++
		winnerIdx = i
		h.circuitRecordSuccess(entry.Backend.Name())
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
			Attempts:          attempts,
		}
		if lastBE != nil {
			rec.ResponseBody = lastBE.Body
		}
		rec.RequestBody = string(req.RawBody)
		h.collector.Record(rec)
		writeError(w, http.StatusBadGateway, "backend error: "+lastErr.Error())
		log.Error().Str("model", originalModel).Str("backend", lastBackend).Int64("latency_ms", rec.LatencyMs).Bool("stream", true).Str("client", clientName).Msg("completion failed: all backends error")
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
		Attempts:          attempts,
	}

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var firstTokenAt time.Time
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
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
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
		log.Error().Err(err).Msg("stream scan error")
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	stats.ComputeTPS(&rec, firstTokenAt, time.Now())
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
	log.Info().Str("model", originalModel).Str("backend", lastBackend).Int64("latency_ms", rec.LatencyMs).Int64("ttft_ms", rec.TTFTMs).Float64("tps", rec.TPS).Bool("fallback", winnerIdx > 0).Bool("stream", true).Str("client", clientName).Msg("completion complete")
}

// raceResult holds the outcome of a single backend attempt in race mode.
type raceResult struct {
	resp      *backend.ChatCompletionResponse
	be        backend.Backend
	err       error
	beErr     *backend.BackendError
	latencyMs int64
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
			t0 := time.Now()
			resp, err := e.Backend.ChatCompletion(ctx, &reqCopy)
			rr := raceResult{resp: resp, be: e.Backend, err: err, latencyMs: time.Since(t0).Milliseconds()}
			errors.As(err, &rr.beErr)
			resultCh <- rr
		}()
	}
	// Close channel once all goroutines finish so we can drain below.
	go func() { wg.Wait(); close(resultCh) }()

	var lastErr error
	var lastBE *backend.BackendError
	var lastBEName string
	var attempts []stats.Attempt
	received := 0
	attemptOrder := 0
	for rr := range resultCh {
		received++
		if rr.err == nil {
			// Winner — cancel all remaining, return response.
			cancel()
			h.circuitRecordSuccess(rr.be.Name())
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
				Attempts:          attempts,
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
			stats.ComputeTPS(&rec, time.Time{}, time.Time{})
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
		// Skip context.Canceled errors from backends cancelled after a winner was found.
		if !errors.Is(rr.err, context.Canceled) {
			a := stats.Attempt{
				AttemptOrder: attemptOrder,
				Backend:      rr.be.Name(),
				Error:        rr.err.Error(),
				LatencyMs:    rr.latencyMs,
			}
			if rr.beErr != nil {
				a.StatusCode = rr.beErr.StatusCode
				a.ResponseBody = rr.beErr.Body
				// Record 429 for circuit breaker.
				if rr.beErr.StatusCode == http.StatusTooManyRequests {
					h.circuitRecord429(rr.be.Name(), rr.beErr)
				}
			}
			attempts = append(attempts, a)
			attemptOrder++
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
		Attempts:          attempts,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	rec.RequestBody = string(req.RawBody)
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
		result    raceStreamResult
		err       error
		be        backend.Backend
		latencyMs int64
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
			t0 := time.Now()
			stream, err := e.Backend.ChatCompletionStream(bCtx, &reqCopy)
			if err != nil {
				resultCh <- streamAttempt{err: err, be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
				return
			}
			// Buffer initial data to confirm the stream is alive before declaring a winner.
			buf := make([]byte, 4096)
			n, readErr := stream.Read(buf)
			if readErr != nil && n == 0 {
				stream.Close()
				resultCh <- streamAttempt{err: fmt.Errorf("stream read failed: %w", readErr), be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
				return
			}
			resultCh <- streamAttempt{result: raceStreamResult{
				stream:   stream,
				buffered: buf[:n],
				be:       e.Backend,
			}, be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var winner *raceStreamResult
	var lastErr error
	var attempts []stats.Attempt
	received := 0
	attemptOrder := 0
	for attempt := range resultCh {
		received++
		if attempt.err == nil && winner == nil {
			// First successful stream — cancel all other backend contexts.
			parentCancel()
			h.circuitRecordSuccess(attempt.result.be.Name())
			w := attempt.result
			winner = &w
			if received == len(entries) {
				break
			}
			// Keep draining so goroutines can finish.
			continue
		}
		if attempt.err != nil && winner == nil && !errors.Is(attempt.err, context.Canceled) {
			lastErr = attempt.err
			attempts = append(attempts, stats.Attempt{
				AttemptOrder: attemptOrder,
				Backend:      attempt.be.Name(),
				Error:        attempt.err.Error(),
				LatencyMs:    attempt.latencyMs,
			})
			attemptOrder++
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
			RequestBody:       string(req.RawBody),
			Attempts:          attempts,
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
		Attempts:          attempts,
	}

	// Replay buffered data first, then stream the rest.
	fullStream := io.MultiReader(bytes.NewReader(winner.buffered), winner.stream)
	scanner := bufio.NewScanner(fullStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var firstTokenAt time.Time
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
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
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
		log.Error().Err(err).Msg("race stream scan error")
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	stats.ComputeTPS(&rec, firstTokenAt, time.Now())
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
			t0 := time.Now()
			resp, err := e.Backend.ChatCompletion(ctx, &reqCopy)
			rr := raceResult{resp: resp, be: e.Backend, err: err, latencyMs: time.Since(t0).Milliseconds()}
			errors.As(err, &rr.beErr)
			resultCh <- rr
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var lastErr error
	var lastBE *backend.BackendError
	var lastBEName string
	var attempts []stats.Attempt
	received := 0
	attemptOrder := 0
	for rr := range resultCh {
		received++
		if rr.err == nil {
			cancel()
			h.circuitRecordSuccess(rr.be.Name())
			latency := time.Since(start).Milliseconds()
			rec := stats.Record{
				Timestamp:         start,
				Backend:            rr.be.Name(),
				Model:              originalModel,
				LatencyMs:          latency,
				StatusCode:         http.StatusOK,
				Stream:            false,
				Client:            clientName,
				Strategy:          config.StrategyStaggeredRace,
				AttemptedBackends: attempted,
				Attempts:          attempts,
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
			stats.ComputeTPS(&rec, time.Time{}, time.Time{})
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
		// Skip context.Canceled errors from backends cancelled by the winner or context timeout.
		if !errors.Is(rr.err, context.Canceled) {
			a := stats.Attempt{
				AttemptOrder: attemptOrder,
				Backend:      rr.be.Name(),
				Error:        rr.err.Error(),
				LatencyMs:    rr.latencyMs,
			}
			if rr.beErr != nil {
				a.StatusCode = rr.beErr.StatusCode
				a.ResponseBody = rr.beErr.Body
				if rr.beErr.StatusCode == http.StatusTooManyRequests {
					h.circuitRecord429(rr.be.Name(), rr.beErr)
				}
			}
			attempts = append(attempts, a)
			attemptOrder++
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
		Attempts:          attempts,
	}
	if lastBE != nil {
		rec.ResponseBody = lastBE.Body
	}
	rec.RequestBody = string(req.RawBody)
	h.collector.Record(rec)
	writeError(w, http.StatusBadGateway, "backend error: "+errMsg)
}

// handleStaggeredRaceStream is the streaming variant of handleStaggeredRaceNonStream.
func (h *Handler) handleStaggeredRaceStream(ctx context.Context, w http.ResponseWriter, entries []backend.RouteEntry, req *backend.ChatCompletionRequest, originalModel string, start time.Time, clientName string, cl *config.ClientConfig, delay time.Duration) {
	attempted := attemptedBackends(entries)
	parentCtx, parentCancel := context.WithCancel(ctx)
	defer parentCancel()

	type streamAttempt struct {
		result    raceStreamResult
		err       error
		be        backend.Backend
		latencyMs int64
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
					resultCh <- streamAttempt{err: bCtx.Err(), be: e.Backend}
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
			t0 := time.Now()
			stream, err := e.Backend.ChatCompletionStream(bCtx, &reqCopy)
			if err != nil {
				resultCh <- streamAttempt{err: err, be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
				return
			}
			buf := make([]byte, 4096)
			n, readErr := stream.Read(buf)
			if readErr != nil && n == 0 {
				stream.Close()
				resultCh <- streamAttempt{err: fmt.Errorf("stream read failed: %w", readErr), be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
				return
			}
			resultCh <- streamAttempt{result: raceStreamResult{
				stream:   stream,
				buffered: buf[:n],
				be:       e.Backend,
			}, be: e.Backend, latencyMs: time.Since(t0).Milliseconds()}
		}()
	}
	go func() { wg.Wait(); close(resultCh) }()

	var winner *raceStreamResult
	var lastErr error
	var attempts []stats.Attempt
	received := 0
	attemptOrder := 0
	for attempt := range resultCh {
		received++
		if attempt.err == nil && winner == nil {
			parentCancel()
			h.circuitRecordSuccess(attempt.result.be.Name())
			res := attempt.result
			winner = &res
			if received == len(entries) {
				break
			}
			continue
		}
		if attempt.err != nil && winner == nil && !errors.Is(attempt.err, context.Canceled) {
			lastErr = attempt.err
			attempts = append(attempts, stats.Attempt{
				AttemptOrder: attemptOrder,
				Backend:      attempt.be.Name(),
				Error:        attempt.err.Error(),
				LatencyMs:    attempt.latencyMs,
			})
			attemptOrder++
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
			RequestBody:       string(req.RawBody),
			Attempts:          attempts,
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
		Attempts:          attempts,
	}

	fullStream := io.MultiReader(bytes.NewReader(winner.buffered), winner.stream)
	scanner := bufio.NewScanner(fullStream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var firstTokenAt time.Time
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
			if firstTokenAt.IsZero() {
				firstTokenAt = time.Now()
			}
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
		log.Error().Err(err).Msg("staggered-race stream scan error")
		rec.Error = err.Error()
	}

	rec.LatencyMs = time.Since(start).Milliseconds()
	stats.ComputeTPS(&rec, firstTokenAt, time.Now())
	rec.StatusCode = http.StatusOK
	h.collector.Record(rec)
}

func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// ?mode=raw returns backend-prefixed model IDs without deduplication,
	// giving the caller full control over which backend handles each request.
	// Default (no mode or mode=flat) returns deduplicated/flattened models
	// with routing metadata so the proxy picks the best backend automatically.
	if r.URL.Query().Get("mode") == "raw" {
		h.listModelsRaw(w, r)
		return
	}

	h.listModelsFlat(w, r)
}

// listModelsFlat returns deduplicated models with routing metadata.
// When multiple backends serve the same base model, their metadata is
// merged and the proxy's routing system determines which backend to use.
func (h *Handler) listModelsFlat(w http.ResponseWriter, r *http.Request) {
	routing := h.cfgMgr.Get().Routing

	type modelEntry struct {
		model    backend.Model
		backends []string // backends that serve this model (insertion order)
	}
	seen := make(map[string]*modelEntry) // keyed by base model ID
	var order []string

	for _, b := range h.registry.All() {
		models, err := b.ListModels(r.Context())
		if err != nil {
			log.Warn().Err(err).Str("backend", b.Name()).Msg("error listing models")
			continue
		}
		for _, m := range models {
			if m.Disabled {
				continue // skip disabled models in the API response
			}
			baseID := strings.TrimPrefix(m.ID, b.Name()+"/")

			if existing, ok := seen[baseID]; ok {
				if m.ContextLength != nil && (existing.model.ContextLength == nil || *m.ContextLength > *existing.model.ContextLength) {
					existing.model.ContextLength = m.ContextLength
				}
				if m.MaxOutputTokens != nil && (existing.model.MaxOutputTokens == nil || *m.MaxOutputTokens > *existing.model.MaxOutputTokens) {
					existing.model.MaxOutputTokens = m.MaxOutputTokens
				}
				capSet := make(map[string]bool, len(existing.model.Capabilities))
				for _, c := range existing.model.Capabilities {
					capSet[c] = true
				}
				for _, c := range m.Capabilities {
					if !capSet[c] {
						existing.model.Capabilities = append(existing.model.Capabilities, c)
					}
				}
				existing.backends = append(existing.backends, b.Name())
			} else {
				mCopy := m
				mCopy.ID = baseID
				if mCopy.OwnedBy == "" {
					mCopy.OwnedBy = b.Name()
				}
				seen[baseID] = &modelEntry{
					model:    mCopy,
					backends: []string{b.Name()},
				}
				order = append(order, baseID)
			}
		}
	}

	allModels := make([]backend.Model, 0, len(order))
	for _, id := range order {
		entry := seen[id]
		m := entry.model

		strategy := routing.Strategy
		if strategy == "" {
			strategy = config.StrategyPriority
		}

		routedBackends := entry.backends
		entries, resolvedStrategy, _, err := h.registry.ResolveRoute(id, routing)
		if err == nil && len(entries) > 0 {
			routedBackends = make([]string, 0, len(entries))
			for _, re := range entries {
				routedBackends = append(routedBackends, re.Backend.Name())
			}
			strategy = resolvedStrategy
		}

		m.AvailableBackends = routedBackends
		m.RoutingStrategy = strategy

		if len(routedBackends) > 1 {
			m.OwnedBy = routedBackends[0]
		}

		allModels = append(allModels, m)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(backend.ModelList{
		Object: "list",
		Data:   allModels,
	})
}

// listModelsRaw returns all models from all backends with backend-prefixed
// IDs (e.g. "openrouter/gpt-4o"). No deduplication or routing metadata is
// applied — the caller selects the exact backend and model.
func (h *Handler) listModelsRaw(w http.ResponseWriter, r *http.Request) {
	var allModels []backend.Model

	for _, b := range h.registry.All() {
		models, err := b.ListModels(r.Context())
		if err != nil {
			log.Warn().Err(err).Str("backend", b.Name()).Msg("error listing models")
			continue
		}
		for _, m := range models {
			if m.Disabled {
				continue // skip disabled models in the API response
			}
			mCopy := m
			// Ensure the ID is backend-prefixed for raw mode.
			if !strings.HasPrefix(mCopy.ID, b.Name()+"/") {
				mCopy.ID = b.Name() + "/" + mCopy.ID
			}
			if mCopy.OwnedBy == "" {
				mCopy.OwnedBy = b.Name()
			}
			mCopy.AvailableBackends = []string{b.Name()}
			mCopy.RoutingStrategy = "direct"
			allModels = append(allModels, mCopy)
		}
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

// Responses handles POST /v1/responses — native Responses API passthrough.
// It resolves the backend from the model field, type-asserts the backend
// implements ResponsesBackend, and forwards the request natively (no translation).
// Backends that do not support the Responses API receive an appropriate error.
func (h *Handler) Responses(w http.ResponseWriter, r *http.Request) {
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

	var req backend.ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.RawBody = body

	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model field is required")
		return
	}

	// Resolve the backend for the given model.
	entries, _, _, err := h.registry.ResolveRoute(req.Model, h.cfgMgr.Get().Routing)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Use the first routed backend.
	if len(entries) == 0 {
		writeError(w, http.StatusBadRequest, "no backend found for model "+req.Model)
		return
	}

	entry := entries[0]

	// Type-assert to ResponsesBackend.
	rb, ok := entry.Backend.(backend.ResponsesBackend)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("backend %q does not support the Responses API", entry.Backend.Name()))
		return
	}

	reqCopy := req
	reqCopy.Model = entry.ModelID

	if req.Stream {
		h.handleResponsesStream(r.Context(), w, rb, &reqCopy, entry.Backend.Name())
	} else {
		h.handleResponsesNonStream(r.Context(), w, rb, &reqCopy, entry.Backend.Name())
	}
}

// handleResponsesNonStream forwards a non-streaming Responses API request natively.
func (h *Handler) handleResponsesNonStream(ctx context.Context, w http.ResponseWriter, rb backend.ResponsesBackend, req *backend.ResponsesRequest, backendName string) {
	resp, err := rb.Responses(ctx, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, "backend error: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(resp.Body)
}

// handleResponsesStream forwards a streaming Responses API request natively.
// The raw SSE stream from the upstream is piped directly to the client.
func (h *Handler) handleResponsesStream(ctx context.Context, w http.ResponseWriter, rb backend.ResponsesBackend, req *backend.ResponsesRequest, backendName string) {
	stream, err := rb.ResponsesStream(ctx, req)
	if err != nil {
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

	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()

		if ctx.Err() != nil {
			break
		}

		fmt.Fprintf(w, "%s\n", line)
		if line == "" {
			// Blank line signals end of SSE event — flush.
			flusher.Flush()
		}
	}

	if err := scanner.Err(); err != nil {
		log.Printf("responses stream scan error: %v", err)
	}
}

// filterCircuitEntries removes backends with tripped circuit breakers.
// If all would be filtered, returns the original slice (never blocks everything).
func (h *Handler) filterCircuitEntries(entries []backend.RouteEntry) []backend.RouteEntry {
	if h.circuit == nil || !h.circuit.Enabled() {
		return entries
	}

	var active []backend.RouteEntry
	for _, e := range entries {
		if !h.circuit.IsOpen(e.Backend.Name()) {
			active = append(active, e)
		}
	}

	if len(active) == 0 {
		log.Warn().Msg("circuit: all backends tripped, ignoring breakers")
		return entries
	}

	if len(active) < len(entries) {
		skipped := len(entries) - len(active)
		log.Debug().Int("skipped", skipped).Int("active", len(active)).Msg("circuit: filtered tripped backends")
	}

	return active
}

// circuitRecord429 records a 429 response and attempts to extract a Retry-After hint
// from the backend error body.
func (h *Handler) circuitRecord429(backendName string, be *backend.BackendError) {
	if h.circuit == nil {
		return
	}
	h.circuit.Record429(backendName, 0)
}

// circuitRecordSuccess records a successful response.
func (h *Handler) circuitRecordSuccess(backendName string) {
	if h.circuit == nil {
		return
	}
	h.circuit.RecordSuccess(backendName)
}
