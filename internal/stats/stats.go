package stats

import (
	"sync"
	"time"
)

// Record represents a single proxied request.
type Record struct {
	Timestamp        time.Time `json:"timestamp"`
	Backend          string    `json:"backend"`
	Model            string    `json:"model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	CachedTokens     int       `json:"cached_tokens"`
	ReasoningTokens  int       `json:"reasoning_tokens"`
	LatencyMs        int64     `json:"latency_ms"`
	TTFTMs           int64     `json:"ttft_ms,omitempty"`       // Time-to-first-token (streaming only)
	GenerationMs     int64     `json:"generation_ms,omitempty"` // Generation phase duration (excludes TTFT)
	TPS              float64   `json:"tps,omitempty"`           // Tokens per second (completion tokens / generation seconds)
	StatusCode       int       `json:"status_code"`
	Error            string    `json:"error,omitempty"`
	Stream           bool      `json:"stream"`
	ResponseBody     string    `json:"response_body,omitempty"`
	RequestBody      string    `json:"request_body,omitempty"`
	Client           string    `json:"client,omitempty"`
	ID               int64     `json:"id"`

	// Routing metadata (set when multiple backends are configured for the model).
	Strategy          string `json:"strategy,omitempty"`           // e.g. "priority", "round-robin", "race"
	AttemptedBackends string `json:"attempted_backends,omitempty"` // comma-separated, e.g. "zai-coding,zen"
	Fallback          bool   `json:"fallback,omitempty"`           // true if winning backend was not the first attempted

	// Attempts holds per-backend attempt details for fallback tracing.
	// Transient: populated by the handler, persisted to request_attempts table by Save(), not loaded back into Record.
	Attempts []Attempt `json:"-"`
}

// Attempt captures the outcome of a single backend attempt during routing.
type Attempt struct {
	AttemptOrder int    `json:"attempt_order"`
	Backend      string `json:"backend"`
	Error        string `json:"error"`
	StatusCode   int    `json:"status_code"`
	LatencyMs    int64  `json:"latency_ms"`
	ResponseBody string `json:"response_body,omitempty"`
}

// StatsFilter narrows analytics queries.
type StatsFilter struct {
	From    time.Time
	To      time.Time
	Backend string
	Model   string
	Client  string
	ErrOnly bool
}

// TimePoint is one time-bucket in a time-series query.
type TimePoint struct {
	BucketTime   time.Time `json:"t"`
	Requests     int       `json:"req"`
	Tokens       int       `json:"tok"`
	Errors       int       `json:"err"`
	AvgLatencyMs int64     `json:"lat"`
	AvgTPS       float64   `json:"tps"`
}

// Percentiles holds latency distribution values.
type Percentiles struct {
	P50 int64 `json:"p50"`
	P90 int64 `json:"p90"`
	P99 int64 `json:"p99"`
}

// RankRow holds aggregated stats for one dimension value (model/backend/client).
type RankRow struct {
	Name     string  `json:"name"`
	Requests int     `json:"req"`
	Tokens   int     `json:"tok"`
	Errors   int     `json:"err"`
	AvgLatMs int64   `json:"lat"`
	ErrPct   float64 `json:"err_pct"`
	P50      int64   `json:"p50"`
	P90      int64   `json:"p90"`
	P99      int64   `json:"p99"`
}

// Summary provides aggregated statistics.
type Summary struct {
	TotalRequests   int            `json:"total_requests"`
	TotalTokens     int            `json:"total_tokens"`
	TotalErrors     int            `json:"total_errors"`
	AvgLatencyMs    int64          `json:"avg_latency_ms"`
	AvgTPS          float64        `json:"avg_tps"`
	TotalCached     int            `json:"total_cached"`
	TotalReasoning  int            `json:"total_reasoning"`
	ByBackend       map[string]int `json:"by_backend"`
	ByModel         map[string]int `json:"by_model"`
	TokensByBackend map[string]int `json:"tokens_by_backend"`
	ErrorsByBackend map[string]int `json:"errors_by_backend"`
	ByClient        map[string]int `json:"by_client"`
	TokensByClient  map[string]int `json:"tokens_by_client"`
}

// ComputeTPS fills TTFTMs, GenerationMs, and TPS on a record.
// firstTokenAt is the time the first data chunk arrived; now is when the response finished.
// Reasoning tokens are excluded from the TPS numerator since they are generated during
// the "thinking" phase (included in TTFT) and not during the output streaming phase.
// A minimum generation time of 100ms is required to avoid unreliable measurements caused
// by network burst delivery (provider buffering tokens then flushing them over TCP).
func ComputeTPS(rec *Record, firstTokenAt, now time.Time) {
	if !firstTokenAt.IsZero() {
		rec.TTFTMs = firstTokenAt.Sub(rec.Timestamp).Milliseconds()
		if rec.TTFTMs < 0 {
			rec.TTFTMs = 0
		}
		rec.GenerationMs = now.Sub(firstTokenAt).Milliseconds()
		if rec.GenerationMs < 0 {
			rec.GenerationMs = 0
		}
	}

	// Use output tokens only (exclude reasoning tokens that are generated during TTFT).
	outputTokens := rec.CompletionTokens - rec.ReasoningTokens
	if outputTokens <= 0 {
		outputTokens = rec.CompletionTokens
	}

	const minGenerationMs = 100 // avoid noisy TPS from burst-delivered chunks
	if rec.GenerationMs >= minGenerationMs && outputTokens > 0 {
		rec.TPS = float64(outputTokens) / (float64(rec.GenerationMs) / 1000.0)
	} else if rec.LatencyMs > 0 && outputTokens > 0 {
		// Non-streaming fallback or generation too short: use overall latency.
		rec.GenerationMs = rec.LatencyMs
		rec.TPS = float64(outputTokens) / (float64(rec.LatencyMs) / 1000.0)
	}
}

// Collector stores request records in memory.
type Collector struct {
	mu      sync.RWMutex
	records []Record
	maxSize int
	store   *Store
}

func NewCollector(maxSize int) *Collector {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &Collector{
		records: make([]Record, 0, maxSize),
		maxSize: maxSize,
	}
}

// SetStore attaches a persistent store; every subsequent Record call will also write to it.
func (c *Collector) SetStore(s *Store) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = s
}

func (c *Collector) Record(r Record) {
	c.mu.Lock()
	if len(c.records) >= c.maxSize {
		drop := c.maxSize / 10
		if drop == 0 {
			drop = 1
		}
		c.records = c.records[drop:]
	}
	c.records = append(c.records, r)
	store := c.store
	c.mu.Unlock()

	if store != nil {
		store.Save(r)
	}
}

// Recent returns the last n records, newest first.
func (c *Collector) Recent(n int) []Record {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := len(c.records)
	if n > total {
		n = total
	}
	result := make([]Record, n)
	for i := 0; i < n; i++ {
		result[i] = c.records[total-1-i]
	}
	return result
}

// RecentPaged returns one page of records (newest first) and the total count.
// Page is 0-indexed; pageSize is the number of records per page.
func (c *Collector) RecentPaged(page, pageSize int) ([]Record, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	total := len(c.records)
	offset := page * pageSize
	if offset >= total {
		return nil, total
	}
	end := total - offset
	start := end - pageSize
	if start < 0 {
		start = 0
	}
	result := make([]Record, end-start)
	for i := range result {
		result[i] = c.records[end-1-i]
	}
	return result, total
}

// TotalCount returns the number of records held in memory.
func (c *Collector) TotalCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.records)
}

// Clear removes all in-memory records and clears the persistent store if set.
func (c *Collector) Clear() {
	c.mu.Lock()
	c.records = c.records[:0]
	store := c.store
	c.mu.Unlock()
	if store != nil {
		store.DeleteAll()
	}
}

// DeleteFiltered removes records matching the filter from both the in-memory
// slice and the persistent store.  Returns the number of database rows deleted.
func (c *Collector) DeleteFiltered(f StatsFilter) (int64, error) {
	c.mu.Lock()
	// Remove matching records from in-memory slice.
	filtered := c.records[:0]
	for i := range c.records {
		r := &c.records[i]
		if !f.matchRecord(r) {
			filtered = append(filtered, *r)
		}
	}
	c.records = filtered
	store := c.store
	c.mu.Unlock()

	if store != nil {
		return store.DeleteFiltered(f)
	}
	return int64(len(c.records)), nil // approximate when no store
}

// matchRecord reports whether a record matches the filter.
func (f StatsFilter) matchRecord(r *Record) bool {
	if !f.From.IsZero() && r.Timestamp.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && r.Timestamp.After(f.To) {
		return false
	}
	if f.Backend != "" && r.Backend != f.Backend {
		return false
	}
	if f.Model != "" && r.Model != f.Model {
		return false
	}
	if f.Client != "" && r.Client != f.Client {
		return false
	}
	if f.ErrOnly && r.Error == "" {
		return false
	}
	return true
}

// FilteredPaged returns a page of records (newest first) within an optional time window
// and the total count of matching records. since=0 means all time.
func (c *Collector) FilteredPaged(since time.Duration, page, pageSize int) ([]Record, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var cutoff time.Time
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}

	// Records are stored oldest-first; find the first index inside the window.
	n := len(c.records)
	start := 0
	if !cutoff.IsZero() {
		for start < n && c.records[start].Timestamp.Before(cutoff) {
			start++
		}
	}

	total := n - start
	if total <= 0 {
		return nil, 0
	}

	skip := page * pageSize
	result := make([]Record, 0, pageSize)
	// Iterate newest-first
	for i := n - 1; i >= start; i-- {
		if skip > 0 {
			skip--
			continue
		}
		result = append(result, c.records[i])
		if len(result) >= pageSize {
			break
		}
	}
	return result, total
}

// Summarize returns aggregate stats over a given duration (0 = all time).
func (c *Collector) Summarize(since time.Duration) Summary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}

	s := Summary{
		ByBackend:       make(map[string]int),
		ByModel:         make(map[string]int),
		TokensByBackend: make(map[string]int),
		ErrorsByBackend: make(map[string]int),
		ByClient:        make(map[string]int),
		TokensByClient:  make(map[string]int),
	}

	var totalLatency int64
	var tpsSum float64
	var tpsCount int
	for _, r := range c.records {
		if !cutoff.IsZero() && r.Timestamp.Before(cutoff) {
			continue
		}
		s.TotalRequests++
		s.TotalTokens += r.TotalTokens
		s.TotalCached += r.CachedTokens
		s.TotalReasoning += r.ReasoningTokens
		totalLatency += r.LatencyMs
		s.ByBackend[r.Backend]++
		s.ByModel[r.Model]++
		s.TokensByBackend[r.Backend] += r.TotalTokens
		s.ByClient[r.Client]++
		s.TokensByClient[r.Client] += r.TotalTokens
		if r.Error != "" {
			s.TotalErrors++
			s.ErrorsByBackend[r.Backend]++
		}
		if r.TPS > 0 {
			tpsSum += r.TPS * float64(r.CompletionTokens)
			tpsCount += r.CompletionTokens
		}
	}

	if s.TotalRequests > 0 {
		s.AvgLatencyMs = totalLatency / int64(s.TotalRequests)
	}
	if tpsCount > 0 {
		s.AvgTPS = tpsSum / float64(tpsCount)
	}
	return s
}

// BackendRoutingStats holds aggregated routing stats for a single backend within a model.
type BackendRoutingStats struct {
	Name      string  `json:"name"`
	Requests  int     `json:"requests"`
	AvgLatMs  int64   `json:"avg_lat_ms"`
	Errors    int     `json:"errors"`
	Fallbacks int     `json:"fallbacks"`
	WinPct    float64 `json:"win_pct"`
}

// ModelRoutingStats holds aggregated routing stats for a single model.
type ModelRoutingStats struct {
	Model        string                `json:"model"`
	Strategy     string                `json:"strategy"`
	Backends     []BackendRoutingStats `json:"backends"`
	TotalReqs    int                   `json:"total_requests"`
	FallbackRate float64               `json:"fallback_rate"`
	AvgLatMs     int64                 `json:"avg_lat_ms"`
}
