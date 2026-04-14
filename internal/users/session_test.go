package users

import (
	"testing"
)

func TestSessionToken_CreateAndParse(t *testing.T) {
	secret := GenerateSessionSecret()
	token, err := CreateSessionToken("admin", secret)
	if err != nil {
		t.Fatalf("CreateSessionToken: %v", err)
	}
	sess, err := ParseSessionToken(token, secret)
	if err != nil {
		t.Fatalf("ParseSessionToken: %v", err)
	}
	if sess.Username != "admin" {
		t.Fatalf("expected admin, got %q", sess.Username)
	}
}

func TestSessionToken_WrongSecret(t *testing.T) {
	s1 := GenerateSessionSecret()
	s2 := GenerateSessionSecret()
	token, _ := CreateSessionToken("admin", s1)
	_, err := ParseSessionToken(token, s2)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}

func TestSessionToken_Tampered(t *testing.T) {
	secret := GenerateSessionSecret()
	token, _ := CreateSessionToken("admin", secret)
	_, err := ParseSessionToken("X"+token[1:], secret)
	if err == nil {
		t.Fatal("expected error for tampered token")
	}
}

func TestSessionToken_InvalidFormat(t *testing.T) {
	_, err := ParseSessionToken("no-dot", GenerateSessionSecret())
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerateSessionSecret(t *testing.T) {
	s1 := GenerateSessionSecret()
	if len(s1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(s1))
	}
}
