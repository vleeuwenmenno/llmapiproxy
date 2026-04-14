package users

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/rs/zerolog/log"
)

// User represents a stored user (never includes the password hash in public APIs).
type User struct {
	ID        int64
	Username  string
	Role      string
	CreatedAt time.Time
}

// UserStore manages the users SQLite database.
type UserStore struct {
	db *sql.DB
}

// OpenUserStore opens (or creates) the users database at the given path.
func OpenUserStore(path string) (*UserStore, error) {
	dsn := path + "?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open users database: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &UserStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate users database: %w", err)
	}

	log.Info().Str("path", path).Msg("users database opened")
	return s, nil
}

// Close closes the underlying database connection.
func (s *UserStore) Close() error {
	return s.db.Close()
}

func (s *UserStore) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			username    TEXT    UNIQUE NOT NULL,
			password_hash TEXT  NOT NULL,
			role        TEXT    NOT NULL DEFAULT 'admin',
			created_at  DATETIME DEFAULT CURRENT_TIMESTAMP
		)
	`)
	return err
}

// CreateUser hashes the password and inserts a new user.
func (s *UserStore) CreateUser(username, password string) error {
	hash, err := HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	_, err = s.db.Exec(
		"INSERT INTO users (username, password_hash) VALUES (?, ?)",
		username, hash,
	)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// Authenticate verifies a username/password combination. Returns the user on success.
func (s *UserStore) Authenticate(username, password string) (*User, error) {
	var u User
	var hash string
	err := s.db.QueryRow(
		"SELECT id, username, password_hash, role, created_at FROM users WHERE username = ?",
		username,
	).Scan(&u.ID, &u.Username, &hash, &u.Role, &u.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query user: %w", err)
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return nil, fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return nil, nil
	}
	return &u, nil
}

// ListUsers returns all users (without password hashes).
func (s *UserStore) ListUsers() ([]User, error) {
	rows, err := s.db.Query("SELECT id, username, role, created_at FROM users ORDER BY created_at")
	if err != nil {
		return nil, fmt.Errorf("query users: %w", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// DeleteUser removes a user by username.
func (s *UserStore) DeleteUser(username string) error {
	res, err := s.db.Exec("DELETE FROM users WHERE username = ?", username)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", username)
	}
	return nil
}

// UserCount returns the number of registered users.
func (s *UserStore) UserCount() (int, error) {
	var count int
	err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count)
	return count, err
}

// ChangePassword updates the password for an existing user.
func (s *UserStore) ChangePassword(username, newPassword string) error {
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	res, err := s.db.Exec("UPDATE users SET password_hash = ? WHERE username = ?", hash, username)
	if err != nil {
		return fmt.Errorf("update password: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %q not found", username)
	}
	return nil
}
