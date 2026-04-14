package users

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHashAndVerifyPassword(t *testing.T) {
	password := "super-secret-123"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "" {
		t.Fatal("hash should not be empty")
	}

	ok, err := VerifyPassword(password, hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Fatal("expected password to verify, got false")
	}

	ok, err = VerifyPassword("wrong-password", hash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if ok {
		t.Fatal("expected wrong password to fail, got true")
	}
}

func TestVerifyPassword_InvalidHash(t *testing.T) {
	_, err := VerifyPassword("test", "not-a-valid-hash")
	if err == nil {
		t.Fatal("expected error for invalid hash format")
	}
}

func TestUserStore_Crud(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "users.db")

	store, err := OpenUserStore(dbPath)
	if err != nil {
		t.Fatalf("OpenUserStore: %v", err)
	}
	defer store.Close()

	if err := store.CreateUser("admin", "password123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	user, err := store.Authenticate("admin", "password123")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user == nil || user.Username != "admin" {
		t.Fatal("expected admin user")
	}

	user, _ = store.Authenticate("admin", "wrong")
	if user != nil {
		t.Fatal("expected nil for wrong password")
	}

	store.CreateUser("bob", "pass456")
	users, _ := store.ListUsers()
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}

	store.DeleteUser("admin")
	count, _ := store.UserCount()
	if count != 1 {
		t.Fatalf("expected 1 user after delete, got %d", count)
	}

	store.ChangePassword("bob", "newpass")
	user, _ = store.Authenticate("bob", "newpass")
	if user == nil {
		t.Fatal("new password should work")
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		t.Fatal("users.db should exist")
	}
}
