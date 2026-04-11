package chat

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
    id            TEXT PRIMARY KEY,
    title         TEXT    NOT NULL DEFAULT '',
    model         TEXT    NOT NULL DEFAULT '',
    system_prompt TEXT    NOT NULL DEFAULT '',
    temperature   REAL    NOT NULL DEFAULT 0.7,
    top_p         REAL    NOT NULL DEFAULT 1.0,
    max_tokens    INTEGER NOT NULL DEFAULT 4096,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role          TEXT    NOT NULL,
    content       TEXT    NOT NULL DEFAULT '',
    tokens        INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    duration_ms   REAL    NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
`

// Session represents a chat session with its configuration.
type Session struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Model        string  `json:"model"`
	SystemPrompt string  `json:"system_prompt"`
	Temperature  float64 `json:"temperature"`
	TopP         float64 `json:"top_p"`
	MaxTokens    int     `json:"max_tokens"`
	CreatedAt    int64   `json:"created_at"`
	UpdatedAt    int64   `json:"updated_at"`
}

// Message represents a single message within a chat session.
type Message struct {
	ID           int64   `json:"id"`
	SessionID    string  `json:"session_id"`
	Role         string  `json:"role"`
	Content      string  `json:"content"`
	Tokens       int     `json:"tokens"`
	PromptTokens int     `json:"prompt_tokens"`
	Model        string  `json:"model"`
	DurationMs   float64 `json:"duration_ms"`
	CreatedAt    int64   `json:"created_at"`
}

// SessionSummary extends Session with aggregate message statistics for list views.
type SessionSummary struct {
	Session
	TotalCompletionTokens int   `json:"total_completion_tokens"`
	TotalPromptTokens     int   `json:"total_prompt_tokens"`
	LastMessageAt         int64 `json:"last_message_at"`
	MessageCount          int   `json:"message_count"`
}

// ChatStore persists chat sessions and messages to a SQLite database.
type ChatStore struct {
	db *sql.DB
}

// OpenChatStore opens (or creates) the SQLite database at path and returns a ChatStore.
func OpenChatStore(path string) (*ChatStore, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("chat: open database: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite is not concurrent for writes

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("chat: create schema: %w", err)
	}

	// Migrate: add prompt_tokens column if it doesn't exist yet.
	_, _ = db.Exec(`ALTER TABLE messages ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`)

	// Enable foreign keys so ON DELETE CASCADE works.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("chat: enable foreign keys: %w", err)
	}

	return &ChatStore{db: db}, nil
}

// Close closes the underlying database.
func (s *ChatStore) Close() error {
	return s.db.Close()
}

// CreateSession creates a new empty chat session with a generated UUID.
func (s *ChatStore) CreateSession() (*Session, error) {
	now := time.Now().UnixMilli()
	session := &Session{
		ID:           uuid.New().String(),
		Title:        "",
		Model:        "",
		SystemPrompt: "",
		Temperature:  0.7,
		TopP:         1.0,
		MaxTokens:    4096,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, title, model, system_prompt, temperature, top_p, max_tokens, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.Title, session.Model, session.SystemPrompt,
		session.Temperature, session.TopP, session.MaxTokens,
		session.CreatedAt, session.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("chat: create session: %w", err)
	}
	return session, nil
}

// GetSession returns a single session by ID.
func (s *ChatStore) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT id, title, model, system_prompt, temperature, top_p, max_tokens, created_at, updated_at
		 FROM sessions WHERE id = ?`, id)
	var sess Session
	if err := row.Scan(
		&sess.ID, &sess.Title, &sess.Model, &sess.SystemPrompt,
		&sess.Temperature, &sess.TopP, &sess.MaxTokens,
		&sess.CreatedAt, &sess.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("chat: get session: %w", err)
	}
	return &sess, nil
}

// ListSessions returns all sessions ordered by most recently updated first.
func (s *ChatStore) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, title, model, system_prompt, temperature, top_p, max_tokens, created_at, updated_at
		 FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("chat: list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(
			&sess.ID, &sess.Title, &sess.Model, &sess.SystemPrompt,
			&sess.Temperature, &sess.TopP, &sess.MaxTokens,
			&sess.CreatedAt, &sess.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("chat: scan session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// ListSessionSummaries returns sessions with aggregate message statistics,
// ordered by time of last message (most recent first).
func (s *ChatStore) ListSessionSummaries() ([]SessionSummary, error) {
	rows, err := s.db.Query(`
		SELECT s.id, s.title, s.model, s.system_prompt, s.temperature, s.top_p, s.max_tokens, s.created_at, s.updated_at,
		       COALESCE(SUM(m.tokens), 0),
		       COALESCE(SUM(m.prompt_tokens), 0),
		       COALESCE(MAX(m.created_at), s.created_at),
		       COUNT(m.id)
		FROM sessions s
		LEFT JOIN messages m ON m.session_id = s.id
		GROUP BY s.id
		ORDER BY COALESCE(MAX(m.created_at), s.created_at) DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("chat: list session summaries: %w", err)
	}
	defer rows.Close()

	var summaries []SessionSummary
	for rows.Next() {
		var ss SessionSummary
		if err := rows.Scan(
			&ss.ID, &ss.Title, &ss.Model, &ss.SystemPrompt,
			&ss.Temperature, &ss.TopP, &ss.MaxTokens,
			&ss.CreatedAt, &ss.UpdatedAt,
			&ss.TotalCompletionTokens, &ss.TotalPromptTokens,
			&ss.LastMessageAt, &ss.MessageCount,
		); err != nil {
			return nil, fmt.Errorf("chat: scan session summary: %w", err)
		}
		summaries = append(summaries, ss)
	}
	return summaries, rows.Err()
}

// UpdateSession updates a session's fields and sets updated_at to now.
func (s *ChatStore) UpdateSession(id, title, model, systemPrompt string, temperature, topP float64, maxTokens int) error {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`UPDATE sessions SET title=?, model=?, system_prompt=?, temperature=?, top_p=?, max_tokens=?, updated_at=?
		 WHERE id=?`,
		title, model, systemPrompt, temperature, topP, maxTokens, now, id,
	)
	if err != nil {
		return fmt.Errorf("chat: update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat: session not found: %s", id)
	}
	return nil
}

// UpdateSessionTitle updates only the title and updated_at timestamp.
func (s *ChatStore) UpdateSessionTitle(id, title string) error {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`UPDATE sessions SET title=?, updated_at=? WHERE id=?`,
		title, now, id,
	)
	if err != nil {
		return fmt.Errorf("chat: update session title: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat: session not found: %s", id)
	}
	return nil
}

// DeleteSession deletes a session and all its messages (cascade).
func (s *ChatStore) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("chat: delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chat: session not found: %s", id)
	}
	return nil
}

// DeleteAllSessions deletes all sessions and their messages.
func (s *ChatStore) DeleteAllSessions() error {
	if _, err := s.db.Exec(`DELETE FROM messages`); err != nil {
		return fmt.Errorf("chat: delete all messages: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return fmt.Errorf("chat: delete all sessions: %w", err)
	}
	return nil
}

// SaveMessage inserts a new message into a session and bumps the session's updated_at.
func (s *ChatStore) SaveMessage(sessionID, role, content string, tokens, promptTokens int, model string, durationMs float64) (*Message, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`INSERT INTO messages (session_id, role, content, tokens, prompt_tokens, model, duration_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, role, content, tokens, promptTokens, model, durationMs, now,
	)
	if err != nil {
		return nil, fmt.Errorf("chat: save message: %w", err)
	}
	id, _ := res.LastInsertId()

	// Bump session updated_at so it sorts to the top of the list.
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at=? WHERE id=?`, now, sessionID); err != nil {
	log.Error().Err(err).Msg("chat: failed to bump session updated_at")
	}

	return &Message{
		ID:           id,
		SessionID:    sessionID,
		Role:         role,
		Content:      content,
		Tokens:       tokens,
		PromptTokens: promptTokens,
		Model:        model,
		DurationMs:   durationMs,
		CreatedAt:    now,
	}, nil
}

// ListMessages returns all messages for a session in chronological order.
func (s *ChatStore) ListMessages(sessionID string) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, tokens, prompt_tokens, model, duration_ms, created_at
		 FROM messages WHERE session_id=? ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("chat: list messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(
			&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
			&msg.Tokens, &msg.PromptTokens, &msg.Model, &msg.DurationMs, &msg.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("chat: scan message: %w", err)
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}
