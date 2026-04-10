package stats

import (
	"database/sql"
	"fmt"
	"log"
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
		    (timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,latency_ms,status_code,error,stream,response_body,client)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
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
		        latency_ms,status_code,error,stream,response_body,client
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
	_, err := s.db.Exec("PRAGMA user_version = 2")
	return err
}

func (s *Store) GetByID(id int64) (*Record, error) {
	row := s.db.QueryRow(
		`SELECT id,timestamp,backend,model,prompt_tokens,completion_tokens,total_tokens,
		        latency_ms,status_code,error,stream,response_body,client
		 FROM requests WHERE id = ?`, id)
	var r Record
	var tsMillis int64
	var stream int
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
	); err != nil {
		return nil, err
	}
	r.Timestamp = time.UnixMilli(tsMillis)
	r.Stream = stream != 0
	return &r, nil
}
