package users

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

const (
	sessionCookieName = "llmproxy_session"
	sessionDuration   = 24 * time.Hour
)

// Session holds the decoded session data.
type Session struct {
	Username string
	ExpiresAt time.Time
}

type sessionPayload struct {
	Username  string `json:"u"`
	ExpiresAt int64  `json:"e"`
	Nonce     string `json:"n"`
}

// CreateSessionToken creates a signed session token for the given username.
func CreateSessionToken(username string, secret []byte) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}

	payload := sessionPayload{
		Username:  username,
		ExpiresAt: time.Now().Add(sessionDuration).Unix(),
		Nonce:     base64.RawStdEncoding.EncodeToString(nonce),
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal payload: %w", err)
	}

	encoded := base64.RawStdEncoding.EncodeToString(payloadBytes)
	sig := sign(encoded, secret)
	return encoded + "." + sig, nil
}

// ParseSessionToken validates and decodes a session token.
func ParseSessionToken(token string, secret []byte) (*Session, error) {
	// Split into payload and signature
	dotIdx := -1
	for i := len(token) - 1; i >= 0; i-- {
		if token[i] == '.' {
			dotIdx = i
			break
		}
	}
	if dotIdx < 0 {
		return nil, fmt.Errorf("invalid token format")
	}

	encoded := token[:dotIdx]
	sig := token[dotIdx+1:]

	// Verify signature
	expectedSig := sign(encoded, secret)
	if !hmac.Equal([]byte(sig), []byte(expectedSig)) {
		return nil, fmt.Errorf("invalid signature")
	}

	// Decode payload
	payloadBytes, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	var payload sessionPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("unmarshal payload: %w", err)
	}

	// Check expiry
	if time.Now().Unix() > payload.ExpiresAt {
		return nil, fmt.Errorf("session expired")
	}

	return &Session{
		Username:  payload.Username,
		ExpiresAt: time.Unix(payload.ExpiresAt, 0),
	}, nil
}

// SessionCookieName returns the name of the session cookie.
func SessionCookieName() string {
	return sessionCookieName
}

// GenerateSessionSecret generates a cryptographically random 32-byte session secret.
func GenerateSessionSecret() []byte {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("failed to generate session secret: " + err.Error())
	}
	return secret
}

// sign computes the HMAC-SHA256 of the given message using the secret.
func sign(message string, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(message))
	return base64.RawStdEncoding.EncodeToString(mac.Sum(nil))
}
