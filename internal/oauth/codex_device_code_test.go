package oauth

import (
	"context"
	"errors"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- Codex Device Code Flow Tests ---

func TestCodexDeviceCode_InitiateDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Errorf("expected application/x-www-form-urlencoded content type")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.FormValue("client_id") != defaultCodexClientID {
			t.Errorf("client_id = %q, want %q", r.FormValue("client_id"), defaultCodexClientID)
		}
		if r.FormValue("scope") != defaultCodexScope {
			t.Errorf("scope = %q, want %q", r.FormValue("scope"), defaultCodexScope)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CodexDeviceCodeResponse{
			DeviceCode:      "DC-codex-test-device-code",
			UserCode:        "ABCD-EFGH",
			VerificationURI: "https://auth.openai.com/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, err := os.MkdirTemp("", "codex-device-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	handler := NewCodexDeviceCodeHandler(ts, nil, WithCodexDeviceCodeURL(server.URL))

	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}

	if resp.DeviceCode != "DC-codex-test-device-code" {
		t.Errorf("DeviceCode = %q, want %q", resp.DeviceCode, "DC-codex-test-device-code")
	}
	if resp.UserCode != "ABCD-EFGH" {
		t.Errorf("UserCode = %q, want %q", resp.UserCode, "ABCD-EFGH")
	}
	if resp.VerificationURI != "https://auth.openai.com/device" {
		t.Errorf("VerificationURI = %q, want %q", resp.VerificationURI, "https://auth.openai.com/device")
	}
	if resp.Interval != 5 {
		t.Errorf("Interval = %d, want %d", resp.Interval, 5)
	}
	if resp.ExpiresIn != 900 {
		t.Errorf("ExpiresIn = %d, want %d", resp.ExpiresIn, 900)
	}
}

func TestCodexDeviceCode_InitiateDeviceCode_StoresPending(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CodexDeviceCodeResponse{
			DeviceCode:      "DC-pending-test",
			UserCode:        "WXYZ-1234",
			VerificationURI: "https://auth.openai.com/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, nil, WithCodexDeviceCodeURL(server.URL))

	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}

	// Verify pending state was stored.
	pending := handler.GetPendingDeviceCode(resp.DeviceCode)
	if pending == nil {
		t.Fatal("pending device code not tracked")
	}
	if pending.UserCode != "WXYZ-1234" {
		t.Errorf("pending UserCode = %q, want %q", pending.UserCode, "WXYZ-1234")
	}

	if !handler.HasPendingFlow() {
		t.Error("HasPendingFlow should return true after initiating")
	}
}

func TestCodexDeviceCode_PollUntilAuthorized(t *testing.T) {
	pollCount := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 3 {
			// Return "authorization_pending" for first 2 polls.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"error":             "authorization_pending",
				"error_description": "The authorization request is still pending.",
			})
			return
		}
		// Third poll succeeds with tokens.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "codex-access-token",
			"refresh_token": "codex-refresh-token",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email offline_access",
		})
	}))
	defer tokenServer.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))

	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID:    defaultCodexClientID,
		TokenURL:    tokenServer.URL,
		RedirectURI: "http://localhost:8000/ui/oauth/callback/codex",
		Scope:       defaultCodexScope,
	})

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode:      "DC-test-code",
		UserCode:        "TEST-1234",
		VerificationURI: "https://auth.openai.com/device",
		ExpiresIn:       900,
		Interval:        1, // fast polling for test
	}

	tokenData, err := handler.WaitForAuthorization(context.Background(), dcResp)
	if err != nil {
		t.Fatalf("WaitForAuthorization: %v", err)
	}

	if tokenData.AccessToken != "codex-access-token" {
		t.Errorf("AccessToken = %q, want %q", tokenData.AccessToken, "codex-access-token")
	}
	if tokenData.RefreshToken != "codex-refresh-token" {
		t.Errorf("RefreshToken = %q, want %q", tokenData.RefreshToken, "codex-refresh-token")
	}
	if tokenData.Source != "codex_device_code" {
		t.Errorf("Source = %q, want %q", tokenData.Source, "codex_device_code")
	}

	// Should have polled exactly 3 times (2 pending + 1 success).
	if pollCount != 3 {
		t.Errorf("pollCount = %d, want 3", pollCount)
	}

	// Verify token was persisted.
	loaded := ts.Get()
	if loaded == nil {
		t.Fatal("token not persisted")
	}
	if loaded.AccessToken != "codex-access-token" {
		t.Errorf("persisted token = %q, want %q", loaded.AccessToken, "codex-access-token")
	}
	if loaded.Source != "codex_device_code" {
		t.Errorf("persisted source = %q, want %q", loaded.Source, "codex_device_code")
	}
}

func TestCodexDeviceCode_PollSlowDown(t *testing.T) {
	pollCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount == 1 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "codex-access",
			"refresh_token": "codex-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID: defaultCodexClientID,
		TokenURL: server.URL,
		Scope:    defaultCodexScope,
	})

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode: "DC-slow-down",
		UserCode:   "SLOW-1234",
		Interval:   1,
	}

	tokenData, err := handler.WaitForAuthorization(context.Background(), dcResp)
	if err != nil {
		t.Fatalf("WaitForAuthorization: %v", err)
	}
	if tokenData.AccessToken != "codex-access" {
		t.Errorf("token = %q, want %q", tokenData.AccessToken, "codex-access")
	}
}

func TestCodexDeviceCode_PollExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "expired_token",
			"error_description": "The device code has expired.",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID: defaultCodexClientID,
		TokenURL: server.URL,
		Scope:    defaultCodexScope,
	})

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode: "DC-expired",
		UserCode:   "EXPI-1234",
		Interval:   1,
	}

	_, err := handler.WaitForAuthorization(context.Background(), dcResp)
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	var dcErr *DeviceCodeError
	if !errors.As(err, &dcErr) || dcErr.Code != "expired_token" {
		t.Errorf("expected DeviceCodeError with expired_token, got %T: %v", err, err)
	}
}

func TestCodexDeviceCode_PollAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "access_denied"})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID: defaultCodexClientID,
		TokenURL: server.URL,
		Scope:    defaultCodexScope,
	})

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode: "DC-denied",
		UserCode:   "DENY-1234",
		Interval:   1,
	}

	_, err := handler.WaitForAuthorization(context.Background(), dcResp)
	if err == nil {
		t.Fatal("expected error for access denied")
	}
}

func TestCodexDeviceCode_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID: defaultCodexClientID,
		TokenURL: server.URL,
		Scope:    defaultCodexScope,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode: "DC-cancel",
		UserCode:   "CANC-1234",
		Interval:   1,
	}

	_, err := handler.WaitForAuthorization(ctx, dcResp)
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
}

func TestCodexDeviceCode_FullFlow(t *testing.T) {
	// Test the complete flow: initiate → poll → tokens persisted.
	deviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CodexDeviceCodeResponse{
			DeviceCode:      "DC-full-flow",
			UserCode:        "FULL-1234",
			VerificationURI: "https://auth.openai.com/device",
			ExpiresIn:       900,
			Interval:        1,
		})
	}))
	defer deviceServer.Close()

	pollCount := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 2 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "full-flow-access",
			"refresh_token": "full-flow-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
			"scope":         "openid profile email offline_access",
		})
	}))
	defer tokenServer.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, &CodexOAuthConfig{
		ClientID:    defaultCodexClientID,
		TokenURL:    tokenServer.URL,
		RedirectURI: "http://localhost:8000/ui/oauth/callback/codex",
		Scope:       defaultCodexScope,
	}, WithCodexDeviceCodeURL(deviceServer.URL))

	// Step 1: Initiate.
	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}
	if resp.UserCode != "FULL-1234" {
		t.Errorf("UserCode = %q, want %q", resp.UserCode, "FULL-1234")
	}

	// Step 2: Poll and get tokens.
	tokenData, err := handler.WaitForAuthorization(context.Background(), resp)
	if err != nil {
		t.Fatalf("WaitForAuthorization: %v", err)
	}

	if tokenData.AccessToken != "full-flow-access" {
		t.Errorf("AccessToken = %q, want %q", tokenData.AccessToken, "full-flow-access")
	}
	if tokenData.RefreshToken != "full-flow-refresh" {
		t.Errorf("RefreshToken = %q, want %q", tokenData.RefreshToken, "full-flow-refresh")
	}

	// Verify token persisted.
	loaded := ts.Get()
	if loaded == nil || loaded.AccessToken != "full-flow-access" {
		t.Errorf("persisted token = %v, want full-flow-access", loaded)
	}

	// Verify pending flow cleaned up.
	if handler.HasPendingFlow() {
		t.Error("pending flow should be cleaned up after authorization")
	}
}

func TestCodexDeviceCode_TokensUsableForRefresh(t *testing.T) {
	// Verify that tokens obtained from device code flow can be refreshed
	// using the same refresh mechanism as PKCE tokens.
	pollCount := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount == 1 {
			// Initial device code poll response.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "initial-access",
				"refresh_token": "initial-refresh",
				"token_type":    "Bearer",
				"expires_in":    3600,
			})
			return
		}
		// Subsequent refresh call.
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		if r.FormValue("grant_type") != "refresh_token" {
			t.Errorf("refresh grant_type = %q, want refresh_token", r.FormValue("grant_type"))
		}
		if r.FormValue("refresh_token") != "initial-refresh" {
			t.Errorf("refresh_token = %q, want initial-refresh", r.FormValue("refresh_token"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-access",
			"refresh_token": "rotated-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer tokenServer.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	cfg := &CodexOAuthConfig{
		ClientID:    defaultCodexClientID,
		TokenURL:    tokenServer.URL,
		RedirectURI: "http://localhost:8000/ui/oauth/callback/codex",
		Scope:       defaultCodexScope,
	}
	handler := NewCodexDeviceCodeHandler(ts, cfg)

	dcResp := &CodexDeviceCodeResponse{
		DeviceCode: "DC-refresh-test",
		UserCode:   "REFR-1234",
		Interval:   1,
	}

	tokenData, err := handler.WaitForAuthorization(context.Background(), dcResp)
	if err != nil {
		t.Fatalf("WaitForAuthorization: %v", err)
	}
	if tokenData.AccessToken != "initial-access" {
		t.Fatalf("initial token = %q, want initial-access", tokenData.AccessToken)
	}

	// Now use the CodexOAuthHandler to refresh the token.
	oauthHandler := NewCodexOAuthHandler(ts, cfg)
	refreshed, err := oauthHandler.RefreshToken(context.Background())
	if err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if refreshed.AccessToken != "refreshed-access" {
		t.Errorf("refreshed token = %q, want refreshed-access", refreshed.AccessToken)
	}
	if refreshed.RefreshToken != "rotated-refresh" {
		t.Errorf("rotated refresh token = %q, want rotated-refresh", refreshed.RefreshToken)
	}
}

func TestCodexDeviceCode_ConcurrentInitiate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CodexDeviceCodeResponse{
			DeviceCode:      "DC-concurrent",
			UserCode:        "CONC-1234",
			VerificationURI: "https://auth.openai.com/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, nil, WithCodexDeviceCodeURL(server.URL))

	const numGoroutines = 5
	results := make(chan *CodexDeviceCodeResponse, numGoroutines)
	errs := make(chan error, numGoroutines)

	var wg sync.WaitGroup
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := handler.InitiateDeviceCode(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- resp
		}()
	}
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("concurrent initiate failed: %v", err)
	}

	count := 0
	for range results {
		count++
	}
	if count != numGoroutines {
		t.Errorf("got %d results, want %d", count, numGoroutines)
	}
}

func TestCodexDeviceCode_InvalidClientID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_client",
			"error_description": "Client ID not found.",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, nil, WithCodexDeviceCodeURL(server.URL))

	_, err := handler.InitiateDeviceCode(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid client ID")
	}
}

func TestCodexDeviceCode_DefaultIntervalAndExpiry(t *testing.T) {
	// When the response doesn't include interval or expires_in, defaults should be used.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "DC-defaults",
			"user_code":        "DEFA-1234",
			"verification_uri": "https://auth.openai.com/device",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "codex-device-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewCodexDeviceCodeHandler(ts, nil, WithCodexDeviceCodeURL(server.URL))

	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}

	if resp.Interval != codexDefaultPollInterval {
		t.Errorf("Interval = %d, want default %d", resp.Interval, codexDefaultPollInterval)
	}
	if resp.ExpiresIn != codexDeviceCodeExpiry {
		t.Errorf("ExpiresIn = %d, want default %d", resp.ExpiresIn, codexDeviceCodeExpiry)
	}
}
