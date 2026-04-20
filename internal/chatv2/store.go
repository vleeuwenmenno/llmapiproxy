package chatv2

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/rs/zerolog/log"

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
CREATE INDEX IF NOT EXISTS idx_chatv2_sessions_updated ON sessions(updated_at);

CREATE TABLE IF NOT EXISTS messages (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id    TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role          TEXT    NOT NULL,
    content       TEXT    NOT NULL DEFAULT '',
    tokens        INTEGER NOT NULL DEFAULT 0,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    model         TEXT    NOT NULL DEFAULT '',
    duration_ms   REAL    NOT NULL DEFAULT 0,
    tps           REAL    NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chatv2_messages_session ON messages(session_id);

CREATE TABLE IF NOT EXISTS model_defaults (
    model_id      TEXT PRIMARY KEY,
    temperature   REAL    NOT NULL DEFAULT 0.7,
    top_p         REAL    NOT NULL DEFAULT 1.0,
    max_tokens    INTEGER NOT NULL DEFAULT 4096,
    system_prompt TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);
`

// Session represents a chatv2 session with its configuration.
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

// Message represents a single message within a chatv2 session.
type Message struct {
	ID           int64   `json:"id"`
	SessionID    string  `json:"session_id"`
	Role         string  `json:"role"`
	Content      string  `json:"content"`
	Tokens       int     `json:"tokens"`
	PromptTokens int     `json:"prompt_tokens"`
	Model        string  `json:"model"`
	DurationMs   float64 `json:"duration_ms"`
	TPS          float64 `json:"tps"`
	CreatedAt    int64   `json:"created_at"`
}

// ModelDefaults stores per-model parameter overrides.
type ModelDefaults struct {
	ModelID      string  `json:"model_id"`
	Temperature  float64 `json:"temperature"`
	TopP         float64 `json:"top_p"`
	MaxTokens    int     `json:"max_tokens"`
	SystemPrompt string  `json:"system_prompt"`
	UpdatedAt    int64   `json:"updated_at"`
}

// SessionSummary extends Session with aggregate message statistics for list views.
type SessionSummary struct {
	Session
	TotalCompletionTokens int   `json:"total_completion_tokens"`
	TotalPromptTokens     int   `json:"total_prompt_tokens"`
	LastMessageAt         int64 `json:"last_message_at"`
	MessageCount          int   `json:"message_count"`
}

// Store persists chatv2 sessions, messages, and model defaults to a SQLite database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (or creates) the SQLite database at path and returns a Store.
func OpenStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("chatv2: open database: %w", err)
	}
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("chatv2: create schema: %w", err)
	}

	// Enable foreign keys so ON DELETE CASCADE works.
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("chatv2: enable foreign keys: %w", err)
	}

	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// CreateSession creates a new chat session with a generated UUID and the given defaults.
// If model is empty, defaults to "gpt-4o". Temperature defaults to 0.7, TopP to 1.0, MaxTokens to 4096.
func (s *Store) CreateSession(title, model, systemPrompt string, temperature, topP float64, maxTokens int) (*Session, error) {
	now := time.Now().UnixMilli()
	if model == "" {
		model = "gpt-4o"
	}
	if temperature == 0 {
		temperature = 0.7
	}
	if topP == 0 {
		topP = 1.0
	}
	if maxTokens == 0 {
		maxTokens = 4096
	}
	session := &Session{
		ID:           uuid.New().String(),
		Title:        title,
		Model:        model,
		SystemPrompt: systemPrompt,
		Temperature:  temperature,
		TopP:         topP,
		MaxTokens:    maxTokens,
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
		return nil, fmt.Errorf("chatv2: create session: %w", err)
	}
	return session, nil
}

// GetSession returns a single session by ID.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(
		`SELECT id, title, model, system_prompt, temperature, top_p, max_tokens, created_at, updated_at
		 FROM sessions WHERE id = ?`, id)
	var sess Session
	if err := row.Scan(
		&sess.ID, &sess.Title, &sess.Model, &sess.SystemPrompt,
		&sess.Temperature, &sess.TopP, &sess.MaxTokens,
		&sess.CreatedAt, &sess.UpdatedAt,
	); err != nil {
		return nil, fmt.Errorf("chatv2: get session: %w", err)
	}
	return &sess, nil
}

// UpdateSession updates a session's fields and sets updated_at to now.
func (s *Store) UpdateSession(id, title, model, systemPrompt string, temperature, topP float64, maxTokens int) error {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`UPDATE sessions SET title=?, model=?, system_prompt=?, temperature=?, top_p=?, max_tokens=?, updated_at=?
		 WHERE id=?`,
		title, model, systemPrompt, temperature, topP, maxTokens, now, id,
	)
	if err != nil {
		return fmt.Errorf("chatv2: update session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chatv2: session not found: %s", id)
	}
	return nil
}

// DeleteSession removes a session and all its messages (CASCADE).
func (s *Store) DeleteSession(id string) error {
	res, err := s.db.Exec(`DELETE FROM sessions WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("chatv2: delete session: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("chatv2: session not found: %s", id)
	}
	return nil
}

// DeleteAllSessions removes all sessions and their messages.
func (s *Store) DeleteAllSessions() error {
	if _, err := s.db.Exec(`DELETE FROM messages`); err != nil {
		return fmt.Errorf("chatv2: delete all messages: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return fmt.Errorf("chatv2: delete all sessions: %w", err)
	}
	return nil
}

// ListSessions returns sessions ordered by most recent activity (latest message or session creation).
func (s *Store) ListSessions() ([]SessionSummary, error) {
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
		return nil, fmt.Errorf("chatv2: list sessions: %w", err)
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
			return nil, fmt.Errorf("chatv2: scan session summary: %w", err)
		}
		summaries = append(summaries, ss)
	}
	return summaries, rows.Err()
}

// SaveMessage persists a message with role, content, and metadata.
func (s *Store) SaveMessage(sessionID, role, content string, tokens, promptTokens int, model string, durationMs, tps float64) (*Message, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`INSERT INTO messages (session_id, role, content, tokens, prompt_tokens, model, duration_ms, tps, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, role, content, tokens, promptTokens, model, durationMs, tps, now,
	)
	if err != nil {
		return nil, fmt.Errorf("chatv2: save message: %w", err)
	}
	id, _ := res.LastInsertId()

	// Bump session updated_at so it sorts to the top of the list.
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at=? WHERE id=?`, now, sessionID); err != nil {
		log.Error().Err(err).Msg("chatv2: failed to bump session updated_at")
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
		TPS:          tps,
		CreatedAt:    now,
	}, nil
}

// ListMessages returns all messages for a session in chronological order.
func (s *Store) ListMessages(sessionID string) ([]Message, error) {
	rows, err := s.db.Query(
		`SELECT id, session_id, role, content, tokens, prompt_tokens, model, duration_ms, tps, created_at
		 FROM messages WHERE session_id=? ORDER BY created_at ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("chatv2: list messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		if err := rows.Scan(
			&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
			&msg.Tokens, &msg.PromptTokens, &msg.Model, &msg.DurationMs, &msg.TPS, &msg.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("chatv2: scan message: %w", err)
		}
		messages = append(messages, msg)
	}
	return messages, rows.Err()
}

// SearchSessions returns sessions whose title OR message content contains the query string.
func (s *Store) SearchSessions(query string) ([]SessionSummary, error) {
	pattern := "%" + query + "%"
	rows, err := s.db.Query(`
		SELECT DISTINCT s.id, s.title, s.model, s.system_prompt, s.temperature, s.top_p, s.max_tokens, s.created_at, s.updated_at,
		       COALESCE(msg_totals.total_tokens, 0),
		       COALESCE(msg_totals.total_prompt_tokens, 0),
		       COALESCE(msg_totals.last_msg_at, s.created_at),
		       COALESCE(msg_totals.msg_count, 0)
		FROM sessions s
		LEFT JOIN (
		    SELECT session_id,
		           SUM(tokens) AS total_tokens,
		           SUM(prompt_tokens) AS total_prompt_tokens,
		           MAX(created_at) AS last_msg_at,
		           COUNT(id) AS msg_count
		    FROM messages
		    GROUP BY session_id
		) msg_totals ON msg_totals.session_id = s.id
		WHERE s.title LIKE ? OR s.id IN (SELECT DISTINCT session_id FROM messages WHERE content LIKE ?)
		ORDER BY COALESCE(msg_totals.last_msg_at, s.created_at) DESC
	`, pattern, pattern)
	if err != nil {
		return nil, fmt.Errorf("chatv2: search sessions: %w", err)
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
			return nil, fmt.Errorf("chatv2: scan search result: %w", err)
		}
		summaries = append(summaries, ss)
	}
	return summaries, rows.Err()
}

// GetModelDefaults returns stored defaults for a model or empty defaults if none exist.
func (s *Store) GetModelDefaults(modelID string) (*ModelDefaults, error) {
	row := s.db.QueryRow(
		`SELECT model_id, temperature, top_p, max_tokens, system_prompt, updated_at
		 FROM model_defaults WHERE model_id = ?`, modelID)
	var md ModelDefaults
	if err := row.Scan(
		&md.ModelID, &md.Temperature, &md.TopP, &md.MaxTokens,
		&md.SystemPrompt, &md.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			// Return empty defaults with sensible values.
			return &ModelDefaults{
				ModelID:     modelID,
				Temperature: 0.7,
				TopP:        1.0,
				MaxTokens:   4096,
			}, nil
		}
		return nil, fmt.Errorf("chatv2: get model defaults: %w", err)
	}
	return &md, nil
}

// SetModelDefaults persists per-model temperature, top_p, max_tokens, system_prompt.
func (s *Store) SetModelDefaults(modelID string, temperature, topP float64, maxTokens int, systemPrompt string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.Exec(`
		INSERT INTO model_defaults (model_id, temperature, top_p, max_tokens, system_prompt, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(model_id) DO UPDATE SET
			temperature = excluded.temperature,
			top_p = excluded.top_p,
			max_tokens = excluded.max_tokens,
			system_prompt = excluded.system_prompt,
			updated_at = excluded.updated_at`,
		modelID, temperature, topP, maxTokens, systemPrompt, now,
	)
	if err != nil {
		return fmt.Errorf("chatv2: set model defaults: %w", err)
	}
	return nil
}

// ListAllModelDefaults returns all stored model defaults.
func (s *Store) ListAllModelDefaults() ([]ModelDefaults, error) {
	rows, err := s.db.Query(
		`SELECT model_id, temperature, top_p, max_tokens, system_prompt, updated_at
		 FROM model_defaults ORDER BY model_id`)
	if err != nil {
		return nil, fmt.Errorf("chatv2: list model defaults: %w", err)
	}
	defer rows.Close()

	var defaults []ModelDefaults
	for rows.Next() {
		var md ModelDefaults
		if err := rows.Scan(
			&md.ModelID, &md.Temperature, &md.TopP, &md.MaxTokens,
			&md.SystemPrompt, &md.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("chatv2: scan model defaults: %w", err)
		}
		defaults = append(defaults, md)
	}
	return defaults, rows.Err()
}

// ExportSessionMarkdown generates a markdown formatted string of all messages in a session.
func (s *Store) ExportSessionMarkdown(sessionID string) (string, error) {
	sess, err := s.GetSession(sessionID)
	if err != nil {
		return "", err
	}
	msgs, err := s.ListMessages(sessionID)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(sess.Title)
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("*Model: %s | Created: %s*\n\n", sess.Model, time.UnixMilli(sess.CreatedAt).Format(time.RFC3339)))

	if sess.SystemPrompt != "" {
		b.WriteString("## System Prompt\n\n")
		b.WriteString(sess.SystemPrompt)
		b.WriteString("\n\n")
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "user":
			b.WriteString("**You:** ")
		case "assistant":
			b.WriteString("**Assistant:** ")
		case "system":
			b.WriteString("**System:** ")
		default:
			b.WriteString(fmt.Sprintf("**%s:** ", msg.Role))
		}
		b.WriteString(msg.Content)
		b.WriteString("\n\n")
	}

	return b.String(), nil
}

// ExportSessionJSON generates a JSON array of message objects for a session.
func (s *Store) ExportSessionJSON(sessionID string) (string, error) {
	msgs, err := s.ListMessages(sessionID)
	if err != nil {
		return "", err
	}

	type exportMessage struct {
		Role         string  `json:"role"`
		Content      string  `json:"content"`
		Tokens       int     `json:"tokens,omitempty"`
		PromptTokens int     `json:"prompt_tokens,omitempty"`
		Model        string  `json:"model,omitempty"`
		DurationMs   float64 `json:"duration_ms,omitempty"`
		TPS          float64 `json:"tps,omitempty"`
	}

	exportMsgs := make([]exportMessage, len(msgs))
	for i, msg := range msgs {
		exportMsgs[i] = exportMessage{
			Role:         msg.Role,
			Content:      msg.Content,
			Tokens:       msg.Tokens,
			PromptTokens: msg.PromptTokens,
			Model:        msg.Model,
			DurationMs:   msg.DurationMs,
			TPS:          msg.TPS,
		}
	}

	data, err := json.MarshalIndent(exportMsgs, "", "  ")
	if err != nil {
		return "", fmt.Errorf("chatv2: export session json: %w", err)
	}
	return string(data), nil
}

// GenerateTitle generates a title for a session based on its first user message.
// It returns the first line of the first user message, truncated to 80 characters.
func (s *Store) GenerateTitle(sessionID string) (string, error) {
	msgs, err := s.ListMessages(sessionID)
	if err != nil {
		return "", err
	}

	for _, msg := range msgs {
		if msg.Role == "user" && strings.TrimSpace(msg.Content) != "" {
			title := msg.Content
			// Take first line.
			if idx := strings.Index(title, "\n"); idx != -1 {
				title = title[:idx]
			}
			// Truncate to 80 chars.
			runes := []rune(title)
			if len(runes) > 80 {
				title = string(runes[:80]) + "…"
			}
			title = strings.TrimSpace(title)
			if title != "" {
				// Update the session title.
				now := time.Now().UnixMilli()
				_, err := s.db.Exec(`UPDATE sessions SET title=?, updated_at=? WHERE id=?`, title, now, sessionID)
				if err != nil {
					return "", fmt.Errorf("chatv2: generate title: %w", err)
				}
				return title, nil
			}
		}
	}

	return "", fmt.Errorf("chatv2: no user message found to generate title from")
}
