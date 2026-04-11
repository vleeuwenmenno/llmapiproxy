package oauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// helperToken creates a TokenData for testing.
func helperToken(accessToken string, expiresIn time.Duration) *TokenData {
	now := time.Now()
	return &TokenData{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresAt:   now.Add(expiresIn),
		ObtainedAt:  now,
		Source:      "test",
	}
}

// helperExpiredToken creates a TokenData that is already expired.
func helperExpiredToken(accessToken string) *TokenData {
	now := time.Now()
	return &TokenData{
		AccessToken: accessToken,
		TokenType:   "Bearer",
		ExpiresAt:   now.Add(-1 * time.Hour),
		ObtainedAt:  now.Add(-2 * time.Hour),
		Source:      "test",
	}
}

// helperTempDir creates a temporary directory and returns its path and a cleanup function.
func helperTempDir(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tokenstore-test-*")
	if err != nil {
		t.Fatalf("creating temp dir: %v", err)
	}
	return dir, func() { os.RemoveAll(dir) }
}

// --- VAL-TOKEN-013: Token persistence to disk in JSON format ---

func TestTokenStore_SaveAndLoad(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "test-token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	token := helperToken("test-access-token", 1*time.Hour)
	if err := ts.Save(token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify file exists and is valid JSON
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}

	var loaded TokenData
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parsing token file: %v", err)
	}

	if loaded.AccessToken != "test-access-token" {
		t.Errorf("access token = %q, want %q", loaded.AccessToken, "test-access-token")
	}
	if loaded.Source != "test" {
		t.Errorf("source = %q, want %q", loaded.Source, "test")
	}

	// Load into a new TokenStore to verify round-trip
	ts2, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore (second): %v", err)
	}

	got := ts2.Get()
	if got == nil {
		t.Fatal("Get() returned nil after loading from disk")
	}
	if got.AccessToken != "test-access-token" {
		t.Errorf("access token = %q, want %q", got.AccessToken, "test-access-token")
	}
}

// --- VAL-TOKEN-014: Token file permissions set to 0600 ---

func TestTokenStore_FilePermissions(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "perm-token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	token := helperToken("perm-test-token", 1*time.Hour)
	if err := ts.Save(token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("file permissions = %04o, want 0600", perm)
	}
}

// --- VAL-TOKEN-030: Atomic file writes prevent corruption ---

func TestTokenStore_AtomicWrite(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "atomic-token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Save initial token
	token1 := helperToken("token-1", 1*time.Hour)
	if err := ts.Save(token1); err != nil {
		t.Fatalf("Save token1: %v", err)
	}

	// Verify file is valid JSON
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}
	if err := json.Unmarshal(data, &TokenData{}); err != nil {
		t.Fatalf("token file is not valid JSON: %v", err)
	}

	// Overwrite with a new token
	token2 := helperToken("token-2", 2*time.Hour)
	if err := ts.Save(token2); err != nil {
		t.Fatalf("Save token2: %v", err)
	}

	// Verify the file now contains the new token
	data, err = os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading token file after overwrite: %v", err)
	}
	var loaded TokenData
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("token file is not valid JSON after overwrite: %v", err)
	}
	if loaded.AccessToken != "token-2" {
		t.Errorf("access token = %q, want %q", loaded.AccessToken, "token-2")
	}

	// No temp files should be left behind
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading directory: %v", err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

// --- VAL-TOKEN-015: Token persistence directory creation ---

func TestTokenStore_DirectoryCreation(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	// Use a deeply nested path that doesn't exist
	filePath := filepath.Join(dir, "a", "b", "c", "token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore with nested dirs: %v", err)
	}

	token := helperToken("nested-dir-token", 1*time.Hour)
	if err := ts.Save(token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify the file exists
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("token file should exist: %v", err)
	}
}

func TestTokenStore_DirectoryPermissions(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	// Use a subdirectory that doesn't exist
	subDir := filepath.Join(dir, "oauth-tokens")
	filePath := filepath.Join(subDir, "token.json")
	_, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	info, err := os.Stat(subDir)
	if err != nil {
		t.Fatalf("stat directory: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != 0700 {
		t.Errorf("directory permissions = %04o, want 0700", perm)
	}
}

// --- VAL-TOKEN-016: Token loading from disk on startup ---

func TestTokenStore_LoadOnStartup(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "startup-token.json")

	// Write a token file manually
	token := helperToken("preexisting-token", 1*time.Hour)
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		t.Fatalf("marshaling token: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	// Create a new TokenStore — it should load the preexisting token
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil; expected preexisting token to be loaded")
	}
	if got.AccessToken != "preexisting-token" {
		t.Errorf("access token = %q, want %q", got.AccessToken, "preexisting-token")
	}
}

// --- VAL-TOKEN-017: Token loading with expired persisted token ---

func TestTokenStore_ExpiredTokenDetection(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "expired-token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	expiredToken := helperExpiredToken("expired-token")
	if err := ts.Save(expiredToken); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// ValidToken should return nil for expired tokens
	if got := ts.ValidToken(); got != nil {
		t.Error("ValidToken() should return nil for expired tokens")
	}

	// But Get() should still return the expired token
	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil; expected expired token to still be accessible")
	}
	if got.AccessToken != "expired-token" {
		t.Errorf("access token = %q, want %q", got.AccessToken, "expired-token")
	}

	// IsExpired should be true
	if !got.IsExpired() {
		t.Error("IsExpired() should be true for expired token")
	}
}

func TestTokenStore_LoadExpiredTokenFromDisk(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "expired-on-disk.json")

	// Write an expired token file
	token := helperExpiredToken("disk-expired")
	data, err := json.MarshalIndent(token, "", "  ")
	if err != nil {
		t.Fatalf("marshaling token: %v", err)
	}
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		t.Fatalf("writing token file: %v", err)
	}

	// Load into a new store — the expired token should be present but detectable
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// The expired token is loaded
	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil; expired token should still be loaded from disk")
	}

	// But ValidToken returns nil
	if ts.ValidToken() != nil {
		t.Error("ValidToken() should return nil for expired token loaded from disk")
	}
}

// --- VAL-TOKEN-018: Graceful handling of corrupted JSON files ---

func TestTokenStore_CorruptedFile(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "corrupted.json")

	// Write corrupted JSON
	if err := os.WriteFile(filePath, []byte("{{not valid json}}"), 0600); err != nil {
		t.Fatalf("writing corrupted file: %v", err)
	}

	// NewTokenStore should not panic or error — it should log a warning and start fresh
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore with corrupted file should not error: %v", err)
	}

	// Token should be nil
	if ts.Get() != nil {
		t.Error("Get() should return nil for corrupted file")
	}

	// Should be able to save a new token
	token := helperToken("after-corruption", 1*time.Hour)
	if err := ts.Save(token); err != nil {
		t.Fatalf("Save after corruption: %v", err)
	}

	got := ts.Get()
	if got == nil || got.AccessToken != "after-corruption" {
		t.Error("Save should work after corrupted file recovery")
	}
}

func TestTokenStore_EmptyFile(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "empty.json")

	// Write empty file
	if err := os.WriteFile(filePath, []byte(""), 0600); err != nil {
		t.Fatalf("writing empty file: %v", err)
	}

	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore with empty file should not error: %v", err)
	}

	if ts.Get() != nil {
		t.Error("Get() should return nil for empty file")
	}
}

// --- VAL-TOKEN-023: Thread-safe refresh — single refresh in flight ---

func TestTokenStore_RefreshCoordination_SingleRefresh(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "refresh-coord.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Set initial token
	_ = ts.Save(helperToken("initial", 1*time.Hour))

	// First caller starts refresh
	stillValid1, done1, err1 := ts.StartRefresh()
	if err1 != nil {
		t.Fatalf("StartRefresh: %v", err1)
	}
	if !stillValid1 {
		t.Error("first StartRefresh should report token as still valid")
	}
	if done1 == nil {
		t.Fatal("first StartRefresh should return a non-nil done function")
	}

	// Second caller should get nil done (refresh already in progress)
	stillValid2, done2, err2 := ts.StartRefresh()
	if err2 != nil {
		t.Fatalf("second StartRefresh: %v", err2)
	}
	if done2 != nil {
		t.Error("second StartRefresh should return nil done (another refresh in progress)")
	}
	if !stillValid2 {
		t.Error("second StartRefresh should report cached token as still valid")
	}

	// Third caller also gets nil done
	_, done3, _ := ts.StartRefresh()
	if done3 != nil {
		t.Error("third StartRefresh should also return nil done")
	}

	// First caller finishes
	done1()

	// Now a new caller can start refresh
	_, done4, _ := ts.StartRefresh()
	if done4 == nil {
		t.Error("after first refresh completes, new StartRefresh should return non-nil done")
	}
	done4()
}

// --- VAL-TOKEN-024: Concurrent requests use cached token ---

func TestTokenStore_ConcurrentAccess(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "concurrent.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	token := helperToken("concurrent-token", 1*time.Hour)
	if err := ts.Save(token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	const goroutines = 50
	const iterations = 100
	var wg sync.WaitGroup
	errors := make(chan error, goroutines*iterations)

	// Concurrent readers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				got := ts.Get()
				if got == nil {
					errors <- fmt.Errorf("goroutine %d iter %d: Get() returned nil", id, j)
					continue
				}
				if got.AccessToken != "concurrent-token" {
					errors <- fmt.Errorf("goroutine %d iter %d: access token = %q, want %q",
						id, j, got.AccessToken, "concurrent-token")
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}
}

func TestTokenStore_ConcurrentReadWrite(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "rw-concurrent.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	_ = ts.Save(helperToken("initial", 1*time.Hour))

	const goroutines = 20
	var wg sync.WaitGroup
	errors := make(chan error, goroutines*2)

	// Concurrent writers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tok := helperToken(fmt.Sprintf("token-%d", id), 1*time.Hour)
			if err := ts.Save(tok); err != nil {
				errors <- fmt.Errorf("writer %d: Save: %v", id, err)
			}
		}(i)
	}

	// Concurrent readers
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				got := ts.Get()
				if got == nil {
					// This is possible if a writer cleared/overwrote between saves
					continue
				}
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	// Verify final state is valid
	got := ts.Get()
	if got == nil {
		t.Fatal("final Get() returned nil")
	}

	// Verify the persisted file is valid JSON
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading final token file: %v", err)
	}
	var loaded TokenData
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("final token file is not valid JSON: %v", err)
	}
}

// --- VAL-TOKEN-025: Stale token fallback during refresh ---

func TestTokenStore_StaleTokenFallback(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "stale.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Set a token that is near expiry but not yet expired
	nearExpiry := &TokenData{
		AccessToken: "near-expiry-token",
		TokenType:   "Bearer",
		ExpiresAt:   time.Now().Add(2 * time.Minute),
		ObtainedAt:  time.Now().Add(-58 * time.Minute),
		Source:      "test",
	}
	_ = ts.Save(nearExpiry)

	// Start a refresh
	stillValid, done, _ := ts.StartRefresh()
	if !stillValid {
		t.Error("near-expiry token should still be reported as valid")
	}
	if done == nil {
		t.Fatal("StartRefresh should return non-nil done")
	}

	// During the refresh, the stale token should still be usable
	got := ts.ValidToken()
	if got == nil {
		t.Error("ValidToken() should return non-nil for near-expiry token during refresh")
	}

	// Simulate: refresh fails (don't update the token)
	// The stale token should still be usable
	ts.SetRefreshError(fmt.Errorf("transient network error"))
	done() // Signal refresh complete

	// After failed refresh, stale token should still be usable
	got = ts.ValidToken()
	if got == nil {
		t.Error("ValidToken() should still return the stale token after failed refresh")
	}
	if got.AccessToken != "near-expiry-token" {
		t.Errorf("access token = %q, want %q", got.AccessToken, "near-expiry-token")
	}
}

// --- VAL-TOKEN-026: Expired token during refresh failure ---

func TestTokenStore_ExpiredTokenDuringRefreshFailure(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "expired-refresh-fail.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Set an expired token
	expired := helperExpiredToken("expired-token")
	_ = ts.Save(expired)

	// Start a refresh
	stillValid, done, _ := ts.StartRefresh()
	if stillValid {
		t.Error("expired token should not be reported as still valid")
	}
	if done == nil {
		t.Fatal("StartRefresh should return non-nil done even for expired token")
	}

	// Simulate: refresh fails
	ts.SetRefreshError(fmt.Errorf("network error"))
	done()

	// After failed refresh with expired token, ValidToken should return nil
	got := ts.ValidToken()
	if got != nil {
		t.Error("ValidToken() should return nil when token is expired and refresh failed")
	}
}

// --- VAL-TOKEN-033: Transient write failures handled gracefully ---

func TestTokenStore_TransientWriteFailure(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "transient.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	// Save an initial token
	token1 := helperToken("initial", 1*time.Hour)
	_ = ts.Save(token1)

	// Make the path unwritable by removing the directory
	os.RemoveAll(dir)

	// Attempt to save a new token — should fail but not crash
	token2 := helperToken("after-failure", 1*time.Hour)
	err = ts.Save(token2)
	if err == nil {
		t.Log("Save to removed directory unexpectedly succeeded (directory may have been recreated)")
	}

	// The in-memory token should still reflect the new value
	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil after failed Save; in-memory cache should be updated")
	}
	if got.AccessToken != "after-failure" {
		t.Errorf("access token = %q, want %q (in-memory should be updated even if disk write fails)",
			got.AccessToken, "after-failure")
	}
}

// --- VAL-TOKEN-036: Clock skew tolerance ---

func TestTokenData_IsExpired_ClockSkew(t *testing.T) {
	// Token that expires in 15 seconds — within the 30-second margin
	token := &TokenData{
		AccessToken: "almost-expired",
		ExpiresAt:   time.Now().Add(15 * time.Second),
		ObtainedAt:  time.Now().Add(-1 * time.Hour),
	}

	if !token.IsExpired() {
		t.Error("token expiring within 30s margin should be treated as expired")
	}

	// Token that expires in 60 seconds — outside the margin
	token2 := &TokenData{
		AccessToken: "not-expired",
		ExpiresAt:   time.Now().Add(60 * time.Second),
		ObtainedAt:  time.Now().Add(-1 * time.Hour),
	}

	if token2.IsExpired() {
		t.Error("token expiring in 60s should not be treated as expired")
	}
}

func TestTokenData_IsExpired_NilToken(t *testing.T) {
	var token *TokenData
	if !token.IsExpired() {
		t.Error("nil token should be treated as expired")
	}
}

// --- VAL-TOKEN-037: Token file deleted externally at runtime ---

func TestTokenStore_FileDeletedExternally(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "external-delete.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	token := helperToken("survive-delete", 1*time.Hour)
	_ = ts.Save(token)

	// Delete the file externally
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("removing token file: %v", err)
	}

	// In-memory token should still be available
	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil after external file deletion")
	}
	if got.AccessToken != "survive-delete" {
		t.Errorf("access token = %q, want %q", got.AccessToken, "survive-delete")
	}

	// Save a new token — file should be recreated
	token2 := helperToken("recreated", 1*time.Hour)
	if err := ts.Save(token2); err != nil {
		t.Fatalf("Save after external deletion: %v", err)
	}

	// Verify file was recreated
	if _, err := os.Stat(filePath); err != nil {
		t.Fatalf("token file should have been recreated: %v", err)
	}
}

// --- TokenData.NeedsRefresh tests ---

func TestTokenData_NeedsRefresh_WithRefreshIn(t *testing.T) {
	now := time.Now()
	token := &TokenData{
		AccessToken: "test",
		ExpiresAt:   now.Add(1 * time.Hour),
		ObtainedAt:  now.Add(-30 * time.Minute),
		RefreshIn:   1200, // 20 minutes = 1200 seconds, obtained 30 min ago → should need refresh
		Source:      "test",
	}

	if !token.NeedsRefresh() {
		t.Error("token should need refresh (obtained 30 min ago, refresh_in = 1200s = 20 min)")
	}
}

func TestTokenData_NeedsRefresh_Default80Percent(t *testing.T) {
	now := time.Now()
	// Token obtained 50 minutes ago with 1-hour TTL → 80% threshold at 48 min → needs refresh
	token := &TokenData{
		AccessToken: "test",
		ExpiresAt:   now.Add(10 * time.Minute),
		ObtainedAt:  now.Add(-50 * time.Minute),
		Source:      "test",
	}

	if !token.NeedsRefresh() {
		t.Error("token past 80% of TTL should need refresh")
	}
}

func TestTokenData_NeedsRefresh_NotYet(t *testing.T) {
	now := time.Now()
	// Token obtained 5 minutes ago with 1-hour TTL → 80% threshold at 48 min → does NOT need refresh
	token := &TokenData{
		AccessToken: "test",
		ExpiresAt:   now.Add(55 * time.Minute),
		ObtainedAt:  now.Add(-5 * time.Minute),
		RefreshIn:   3600, // 1 hour
		Source:      "test",
	}

	if token.NeedsRefresh() {
		t.Error("token obtained 5 min ago with refresh_in=3600 should not need refresh yet")
	}
}

func TestTokenData_NeedsRefresh_NilToken(t *testing.T) {
	var token *TokenData
	if !token.NeedsRefresh() {
		t.Error("nil token should need refresh")
	}
}

// --- Clear tests ---

func TestTokenStore_Clear(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "clear.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	_ = ts.Save(helperToken("to-clear", 1*time.Hour))

	if err := ts.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if ts.Get() != nil {
		t.Error("Get() should return nil after Clear()")
	}

	if _, err := os.Stat(filePath); !os.IsNotExist(err) {
		t.Error("token file should be deleted after Clear()")
	}
}

// --- Save with refresh token ---

// helperTokenWithRefresh creates a TokenData with refresh token for testing.
func helperTokenWithRefresh(t *testing.T) *TokenData {
	t.Helper()
	return helperTokenFields(t.Name(), "Bearer", "openid profile")
}

// helperTokenFields builds a complete token with refresh fields.
func helperTokenFields(name, typ, scope string) *TokenData {
	now := time.Now()
	tok := &TokenData{
		TokenType:  typ,
		ExpiresAt:  now.Add(1 * time.Hour),
		ObtainedAt: now,
		Scope:      scope,
		Source:     "oauth",
	}
	// Set token fields individually
	at := name + "-at" // test fixture
	rt := name + "-rt" // test fixture
	tok.AccessToken = at
	tok.RefreshToken = rt
	return tok
}

func TestTokenStore_SaveWithRefreshToken(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "refresh-token.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	token := helperTokenWithRefresh(t)

	if err := ts.Save(token); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify round-trip
	ts2, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore (second): %v", err)
	}

	got := ts2.Get()
	if got == nil {
		t.Fatal("Get() returned nil")
	}
	wantRT := t.Name() + "-rt"
	if got.RefreshToken != wantRT {
		t.Errorf("refresh token = %q, want %q", got.RefreshToken, wantRT)
	}
	if got.Scope != "openid profile" {
		t.Errorf("scope = %q, want %q", got.Scope, "openid profile")
	}
}

// --- Integration: concurrent refresh coordination with saves ---

func TestTokenStore_ConcurrentRefreshCoordination(t *testing.T) {
	dir, cleanup := helperTempDir(t)
	defer cleanup()

	filePath := filepath.Join(dir, "concurrent-refresh.json")
	ts, err := NewTokenStore(filePath)
	if err != nil {
		t.Fatalf("NewTokenStore: %v", err)
	}

	_ = ts.Save(helperToken("initial", 1*time.Hour))

	const numGoroutines = 10
	var wg sync.WaitGroup
	refreshCount := int64(0)
	var mu sync.Mutex

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			_, done, _ := ts.StartRefresh()
			if done == nil {
				// Another goroutine is refreshing — just use the cached token
				return
			}
			defer done()

			mu.Lock()
			refreshCount++
			mu.Unlock()

			// Simulate a refresh
			newToken := helperToken(fmt.Sprintf("refreshed-by-%d", id), 1*time.Hour)
			_ = ts.Save(newToken)
		}(i)
	}

	wg.Wait()

	// Verify that at least one refresh happened but never more than numGoroutines
	if refreshCount == 0 {
		t.Error("expected at least one refresh to occur")
	}
	if refreshCount > int64(numGoroutines) {
		t.Errorf("refresh count %d exceeds goroutine count %d", refreshCount, numGoroutines)
	}

	// Verify the final state is consistent
	got := ts.Get()
	if got == nil {
		t.Fatal("Get() returned nil after concurrent refreshes")
	}

	// Verify the persisted file is valid
	data, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatalf("reading token file: %v", err)
	}
	var loaded TokenData
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("token file is not valid JSON: %v", err)
	}
}
