package stats

import (
	"database/sql"
	"log"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
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

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}

	s := &Store{db: db}
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
		    (timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,latency_ms,status_code,error,stream)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
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
		`SELECT timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,
		        latency_ms,status_code,error,stream
		 FROM requests ORDER BY timestamp ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r Record
		var tsMillis int64
		var stream int
		if err := rows.Scan(
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
		); err != nil {
			return err
		}
		r.Timestamp = time.UnixMilli(tsMillis)
		r.Stream = stream != 0
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
