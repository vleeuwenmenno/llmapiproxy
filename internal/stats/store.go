package stats

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const baseSchema = `
CREATE TABLE IF NOT EXISTS requests (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp         INTEGER NOT NULL,
    backend           TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens      INTEGER NOT NULL DEFAULT 0,
    latency_ms        INTEGER NOT NULL DEFAULT 0,
    status_code       INTEGER NOT NULL DEFAULT 0,
    error             TEXT    NOT NULL DEFAULT '',
    stream            INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_timestamp ON requests(timestamp);
`

// Store persists records to a SQLite database and replays them into a Collector on open.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) the SQLite database at path and returns a Store.
// It also loads all existing records into the supplied Collector.
func OpenStore(path string, c *Collector) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite is not concurrent for writes

	if _, err := db.Exec(baseSchema); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadInto(c); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Save persists a single record to the database.
func (s *Store) Save(r Record) {
	_, err := s.db.Exec(
		`INSERT INTO requests
		    (timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,latency_ms,status_code,error,stream,response_body,client,strategy,attempted_backends,fallback)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.Timestamp.UnixMilli(),
		r.Backend,
		r.Model,
		r.PromptTokens,
		r.CompletionTokens,
		r.TotalTokens,
		r.LatencyMs,
		r.StatusCode,
		r.Error,
		boolToInt(r.Stream),
		r.ResponseBody,
		r.Client,
		r.Strategy,
		r.AttemptedBackends,
		boolToInt(r.Fallback),
	)
	if err != nil {
		log.Printf("stats: failed to save record: %v", err)
	}
}

// DeleteAll removes every record from the database.
func (s *Store) DeleteAll() {
	if _, err := s.db.Exec(`DELETE FROM requests`); err != nil {
		log.Printf("stats: failed to clear records: %v", err)
	}
}

// Prune deletes records older than the given duration to keep the file size in check.
// Call periodically (e.g. daily) if desired.
func (s *Store) Prune(olderThan time.Duration) error {
	cutoff := time.Now().Add(-olderThan).UnixMilli()
	_, err := s.db.Exec(`DELETE FROM requests WHERE timestamp < ?`, cutoff)
	return err
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) loadInto(c *Collector) error {
	rows, err := s.db.Query(
		`SELECT id,timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,
		        latency_ms,status_code,error,stream,response_body,client,
		        strategy,attempted_backends,fallback
		 FROM requests ORDER BY timestamp ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r Record
		var tsMillis int64
		var stream, fallback int
		if err := rows.Scan(
			&r.ID,
			&tsMillis,
			&r.Backend,
			&r.Model,
			&r.PromptTokens,
			&r.CompletionTokens,
			&r.TotalTokens,
			&r.LatencyMs,
			&r.StatusCode,
			&r.Error,
			&stream,
			&r.ResponseBody,
			&r.Client,
			&r.Strategy,
			&r.AttemptedBackends,
			&fallback,
		); err != nil {
			return err
		}
		r.Timestamp = time.UnixMilli(tsMillis)
		r.Stream = stream != 0
		r.Fallback = fallback != 0
		c.Record(r)
	}
	return rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) migrate() error {
	var version int
	_ = s.db.QueryRow("PRAGMA user_version").Scan(&version)
	if version < 1 {
		if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN response_body TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migration 1: %w", err)
		}
	}
	if version < 2 {
		if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN client TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migration 2: %w", err)
		}
	}
	if version < 3 {
		if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN strategy TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migration 3: %w", err)
		}
	}
	if version < 4 {
		if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN attempted_backends TEXT NOT NULL DEFAULT ''"); err != nil {
			return fmt.Errorf("migration 4: %w", err)
		}
	}
	if version < 5 {
		if _, err := s.db.Exec("ALTER TABLE requests ADD COLUMN fallback INTEGER NOT NULL DEFAULT 0"); err != nil {
			return fmt.Errorf("migration 5: %w", err)
		}
	}
	_, err := s.db.Exec("PRAGMA user_version = 5")
	return err
}

func (s *Store) GetByID(id int64) (*Record, error) {
	row := s.db.QueryRow(
		`SELECT id,timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,
		        latency_ms,status_code,error,stream,response_body,client,
		        strategy,attempted_backends,fallback
		 FROM requests WHERE id = ?`, id)
	var r Record
	var tsMillis int64
	var stream, fallback int
	if err := row.Scan(
		&r.ID,
		&tsMillis,
		&r.Backend,
		&r.Model,
		&r.PromptTokens,
		&r.CompletionTokens,
		&r.TotalTokens,
		&r.LatencyMs,
		&r.StatusCode,
		&r.Error,
		&stream,
		&r.ResponseBody,
		&r.Client,
		&r.Strategy,
		&r.AttemptedBackends,
		&fallback,
	); err != nil {
		return nil, err
	}
	r.Timestamp = time.UnixMilli(tsMillis)
	r.Stream = stream != 0
	r.Fallback = fallback != 0
	return &r, nil
}

// buildWhere converts a StatsFilter into a SQL WHERE clause and positional args.
func buildWhere(f StatsFilter) (string, []any) {
	var parts []string
	var args []any
	if !f.From.IsZero() {
		parts = append(parts, "timestamp >= ?")
		args = append(args, f.From.UnixMilli())
	}
	if !f.To.IsZero() {
		parts = append(parts, "timestamp <= ?")
		args = append(args, f.To.UnixMilli())
	}
	if f.Backend != "" {
		parts = append(parts, "backend = ?")
		args = append(args, f.Backend)
	}
	if f.Model != "" {
		parts = append(parts, "model = ?")
		args = append(args, f.Model)
	}
	if f.Client != "" {
		parts = append(parts, "client = ?")
		args = append(args, f.Client)
	}
	if f.ErrOnly {
		parts = append(parts, "error != ''")
	}
	if len(parts) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

// DistinctValues returns the distinct non-empty values for a column (backend, model, or client).
// Returns an empty slice when the store is nil.
func (s *Store) DistinctValues(col string) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	// col is validated by callers — only backend/model/client are passed.
	rows, err := s.db.Query(
		`SELECT DISTINCT ` + col + ` FROM requests WHERE ` + col + ` != '' ORDER BY ` + col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// FilteredSummary returns aggregate statistics for records matching f.
func (s *Store) FilteredSummary(f StatsFilter) (Summary, error) {
	empty := Summary{
		ByBackend:       make(map[string]int),
		ByModel:         make(map[string]int),
		TokensByBackend: make(map[string]int),
		ErrorsByBackend: make(map[string]int),
		ByClient:        make(map[string]int),
		TokensByClient:  make(map[string]int),
	}
	if s == nil {
		return empty, nil
	}
	where, args := buildWhere(f)
	query := `SELECT COUNT(*), COALESCE(SUM(total_tokens),0),
	                 COALESCE(SUM(CASE WHEN error!='' THEN 1 ELSE 0 END),0),
	                 COALESCE(AVG(latency_ms),0)
	          FROM requests ` + where
	row := s.db.QueryRow(query, args...)
	var sum Summary
	var avgLat float64
	if err := row.Scan(&sum.TotalRequests, &sum.TotalTokens, &sum.TotalErrors, &avgLat); err != nil {
		return empty, err
	}
	sum.AvgLatencyMs = int64(avgLat)
	sum.ByBackend = make(map[string]int)
	sum.ByModel = make(map[string]int)
	sum.TokensByBackend = make(map[string]int)
	sum.ErrorsByBackend = make(map[string]int)
	sum.ByClient = make(map[string]int)
	sum.TokensByClient = make(map[string]int)
	return sum, nil
}

// FilteredPercentiles computes P50/P90/P99 latency for records matching f.
func (s *Store) FilteredPercentiles(f StatsFilter) (Percentiles, error) {
	if s == nil {
		return Percentiles{}, nil
	}
	where, args := buildWhere(f)
	rows, err := s.db.Query(`SELECT latency_ms FROM requests `+where+` ORDER BY latency_ms`, args...)
	if err != nil {
		return Percentiles{}, err
	}
	defer rows.Close()
	var lats []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return Percentiles{}, err
		}
		lats = append(lats, v)
	}
	if err := rows.Err(); err != nil {
		return Percentiles{}, err
	}
	n := len(lats)
	if n == 0 {
		return Percentiles{}, nil
	}
	pct := func(p float64) int64 {
		idx := int(float64(n-1) * p)
		return lats[idx]
	}
	return Percentiles{P50: pct(0.50), P90: pct(0.90), P99: pct(0.99)}, nil
}

// TimeSeries returns bucketed time series data for records matching f.
// bucketSecs is the bucket width in seconds.
func (s *Store) TimeSeries(f StatsFilter, bucketSecs int64) ([]TimePoint, error) {
	if s == nil || bucketSecs <= 0 {
		return nil, nil
	}
	bucketMs := bucketSecs * 1000
	where, args := buildWhere(f)
	// Use fmt.Sprintf to embed the literal bucket width so SQLite can use it in GROUP BY/ORDER BY
	// without repeated positional parameters (SQLite doesn't allow ? in GROUP BY expressions referring
	// to the same slot used in SELECT).
	query := fmt.Sprintf(`
		SELECT (timestamp / %d) * %d,
		       COUNT(*),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(CASE WHEN error!='' THEN 1 ELSE 0 END),0),
		       COALESCE(AVG(latency_ms),0)
		FROM requests %s
		GROUP BY (timestamp / %d)
		ORDER BY (timestamp / %d)`,
		bucketMs, bucketMs, where, bucketMs, bucketMs)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pts []TimePoint
	for rows.Next() {
		var tsMs int64
		var pt TimePoint
		var avgLat float64
		if err := rows.Scan(&tsMs, &pt.Requests, &pt.Tokens, &pt.Errors, &avgLat); err != nil {
			return nil, err
		}
		pt.BucketTime = time.UnixMilli(tsMs)
		pt.AvgLatencyMs = int64(avgLat)
		pts = append(pts, pt)
	}
	return pts, rows.Err()
}

// RankBy returns the top-limit rows ranked by request count for a dimension column.
// dim must be "backend", "model", or "client".
func (s *Store) RankBy(f StatsFilter, dim string, limit int) ([]RankRow, error) {
	if s == nil {
		return nil, nil
	}
	where, args := buildWhere(f)
	query := `
		SELECT ` + dim + `,
		       COUNT(*),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(CASE WHEN error!='' THEN 1 ELSE 0 END),0),
		       COALESCE(AVG(latency_ms),0)
		FROM requests ` + where + `
		GROUP BY ` + dim + `
		ORDER BY COUNT(*) DESC
		LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RankRow
	for rows.Next() {
		var rr RankRow
		var avgLat float64
		if err := rows.Scan(&rr.Name, &rr.Requests, &rr.Tokens, &rr.Errors, &avgLat); err != nil {
			return nil, err
		}
		rr.AvgLatMs = int64(avgLat)
		if rr.Requests > 0 {
			rr.ErrPct = float64(rr.Errors) / float64(rr.Requests) * 100
		}
		out = append(out, rr)
	}
	return out, rows.Err()
}

// FilteredRecords returns a filtered, paginated slice of records (newest first) and the total matching count.
func (s *Store) FilteredRecords(f StatsFilter, page, pageSize int) ([]Record, int, error) {
	if s == nil {
		return nil, 0, nil
	}
	where, args := buildWhere(f)

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	offset := page * pageSize
	queryArgs := append(append([]any(nil), args...), pageSize, offset)
	rows, err := s.db.Query(
		`SELECT id,timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,
		        latency_ms,status_code,error,stream,response_body,client,
		        strategy,attempted_backends,fallback
		 FROM requests `+where+`
		 ORDER BY timestamp DESC
		 LIMIT ? OFFSET ?`,
		queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var records []Record
	for rows.Next() {
		var r Record
		var tsMs int64
		var stream, fallback int
		if err := rows.Scan(
			&r.ID, &tsMs, &r.Backend, &r.Model,
			&r.PromptTokens, &r.CompletionTokens, &r.TotalTokens,
			&r.LatencyMs, &r.StatusCode, &r.Error, &stream,
			&r.ResponseBody, &r.Client,
			&r.Strategy, &r.AttemptedBackends, &fallback,
		); err != nil {
			return nil, 0, err
		}
		r.Timestamp = time.UnixMilli(tsMs)
		r.Stream = stream != 0
		r.Fallback = fallback != 0
		records = append(records, r)
	}
	return records, total, rows.Err()
}

// RoutingStats returns per-model, per-backend aggregated routing statistics.
// Only records that have a strategy set (i.e. came through multi-backend routing) are included.
func (s *Store) RoutingStats(f StatsFilter) ([]ModelRoutingStats, error) {
	if s == nil {
		return nil, nil
	}
	where, args := buildWhere(f)
	// Add extra filter: only records with routing metadata.
	if where == "" {
		where = "WHERE strategy != ''"
	} else {
		where += " AND strategy != ''"
	}

	query := `
		SELECT model, backend, strategy,
		       COUNT(*) AS requests,
		       COALESCE(AVG(latency_ms), 0) AS avg_lat,
		       SUM(CASE WHEN error != '' THEN 1 ELSE 0 END) AS errors,
		       SUM(CASE WHEN fallback = 1 THEN 1 ELSE 0 END) AS fallbacks
		FROM requests
		` + where + `
		GROUP BY model, backend
		ORDER BY model, requests DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		model, backend, strategy string
		reqs, errors, fallbacks  int
		avgLat                   float64
	}
	var raw []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.model, &r.backend, &r.strategy, &r.reqs, &r.avgLat, &r.errors, &r.fallbacks); err != nil {
			return nil, err
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Group by model.
	type modelAcc struct {
		strategy  string
		backends  []BackendRoutingStats
		totalReqs int
		totalLat  float64
		totalFall int
	}
	modelMap := make(map[string]*modelAcc)
	modelOrder := []string{}
	for _, r := range raw {
		acc, ok := modelMap[r.model]
		if !ok {
			acc = &modelAcc{strategy: r.strategy}
			modelMap[r.model] = acc
			modelOrder = append(modelOrder, r.model)
		}
		acc.backends = append(acc.backends, BackendRoutingStats{
			Name:      r.backend,
			Requests:  r.reqs,
			AvgLatMs:  int64(r.avgLat),
			Errors:    r.errors,
			Fallbacks: r.fallbacks,
		})
		acc.totalReqs += r.reqs
		acc.totalLat += r.avgLat * float64(r.reqs)
		acc.totalFall += r.fallbacks
	}

	out := make([]ModelRoutingStats, 0, len(modelOrder))
	for _, m := range modelOrder {
		acc := modelMap[m]
		var avgLat int64
		if acc.totalReqs > 0 {
			avgLat = int64(acc.totalLat / float64(acc.totalReqs))
		}
		var fallbackRate float64
		if acc.totalReqs > 0 {
			fallbackRate = float64(acc.totalFall) / float64(acc.totalReqs) * 100
		}
		// Compute win_pct per backend.
		for i := range acc.backends {
			if acc.totalReqs > 0 {
				acc.backends[i].WinPct = float64(acc.backends[i].Requests) / float64(acc.totalReqs) * 100
			}
		}
		out = append(out, ModelRoutingStats{
			Model:        m,
			Strategy:     acc.strategy,
			Backends:     acc.backends,
			TotalReqs:    acc.totalReqs,
			FallbackRate: fallbackRate,
			AvgLatMs:     avgLat,
		})
	}
	return out, nil
}
