package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// --- Device Code Flow Tests ---

func TestDeviceCodeFlow_InitiateDeviceCode(t *testing.T) {
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
		if r.FormValue("client_id") != "Iv1.b507a08c87ecfe98" {
			t.Errorf("client_id = %q, want %q", r.FormValue("client_id"), "Iv1.b507a08c87ecfe98")
		}
		if r.FormValue("scope") != "" {
			t.Errorf("scope should be empty, got %q", r.FormValue("scope"))
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "DC-test-device-code",
			UserCode:        "ABCD-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, err := os.MkdirTemp("", "device-code-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	handler := NewDeviceCodeHandler(ts, WithDeviceCodeURL(server.URL))

	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}

	if resp.DeviceCode != "DC-test-device-code" {
		t.Errorf("DeviceCode = %q, want %q", resp.DeviceCode, "DC-test-device-code")
	}
	if resp.UserCode != "ABCD-1234" {
		t.Errorf("UserCode = %q, want %q", resp.UserCode, "ABCD-1234")
	}
	if resp.VerificationURI != "https://github.com/login/device" {
		t.Errorf("VerificationURI = %q, want %q", resp.VerificationURI, "https://github.com/login/device")
	}
	if resp.Interval != 5 {
		t.Errorf("Interval = %d, want %d", resp.Interval, 5)
	}
}

func TestDeviceCodeFlow_PollUntilAuthorized(t *testing.T) {
	pollCount := 0
	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 3 {
			// Return "authorization_pending" for first 2 polls
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"error": "authorization_pending",
			})
			return
		}
		// Third poll succeeds
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "gho_test-github-token",
			"token_type":    "bearer",
			"scope":         "",
			"expires_in":    28800,
		})
	}))
	defer githubServer.Close()

	// Mock Copilot exchange server
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      "tid=test-copilot-token;fcv1=1:mac",
		})
	}))
	defer copilotServer.Close()

	dir, err := os.MkdirTemp("", "device-code-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	ts, err := NewTokenStore(filepath.Join(dir, "token.json"))
	if err != nil {
		t.Fatal(err)
	}

	handler := NewDeviceCodeHandler(ts,
		WithDeviceCodeURL("http://fake-device-code.example.com"), // not used in poll
		WithAccessTokenURL(githubServer.URL),
		WithCopilotExchangerURL(copilotServer.URL),
	)

	// Test the poll function directly
	githubToken, err := handler.pollForAccessToken(context.Background(), "DC-test-device-code")
	if err != nil {
		t.Fatalf("pollForAccessToken: %v", err)
	}

	if githubToken != "gho_test-github-token" {
		t.Errorf("access_token = %q, want %q", githubToken, "gho_test-github-token")
	}

	// Should have polled exactly 3 times (2 pending + 1 success)
	if pollCount != 3 {
		t.Errorf("pollCount = %d, want 3", pollCount)
	}
}

func TestDeviceCodeFlow_PollSlowDown(t *testing.T) {
	pollCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount == 1 {
			// Return "slow_down" to increase interval
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"error": "slow_down",
			})
			return
		}
		// Second poll succeeds
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_test-token",
			"token_type":   "bearer",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithAccessTokenURL(server.URL))

	githubToken, err := handler.pollForAccessToken(context.Background(), "DC-test-code")
	if err != nil {
		t.Fatalf("pollForAccessToken: %v", err)
	}

	if githubToken != "gho_test-token" {
		t.Errorf("token = %q, want %q", githubToken, "gho_test-token")
	}
}

func TestDeviceCodeFlow_PollExpired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "expired_token",
			"error_description": "The device code has expired.",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithAccessTokenURL(server.URL))

	_, err := handler.pollForAccessToken(context.Background(), "DC-expired-code")
	if err == nil {
		t.Fatal("expected error for expired token")
	}
	if _, ok := err.(*DeviceCodeError); !ok {
		t.Errorf("expected DeviceCodeError, got %T: %v", err, err)
	}
}

func TestDeviceCodeFlow_PollAccessDenied(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error": "access_denied",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithAccessTokenURL(server.URL))

	_, err := handler.pollForAccessToken(context.Background(), "DC-denied-code")
	if err == nil {
		t.Fatal("expected error for access denied")
	}
}

func TestDeviceCodeFlow_FullFlow(t *testing.T) {
	// This tests the full flow: initiate → poll → exchange → validate
	githubDeviceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "DC-full-flow",
			UserCode:        "WXYZ-5678",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        1,
		})
	}))
	defer githubDeviceServer.Close()

	pollCount := 0
	githubTokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pollCount++
		if pollCount < 2 {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_full-flow-token",
			"token_type":   "bearer",
			"expires_in":   28800,
		})
	}))
	defer githubTokenServer.Close()

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_full-flow-token" {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"message": "Bad credentials"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      "tid=full-flow-copilot-token",
		})
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts,
		WithDeviceCodeURL(githubDeviceServer.URL),
		WithAccessTokenURL(githubTokenServer.URL),
		WithCopilotExchangerURL(copilotServer.URL),
	)

	// Step 1: Initiate
	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}
	if resp.UserCode != "WXYZ-5678" {
		t.Errorf("UserCode = %q, want %q", resp.UserCode, "WXYZ-5678")
	}

	// Step 2: Poll + Exchange + Validate
	tokenData, err := handler.WaitForDeviceAuthorization(context.Background(), resp)
	if err != nil {
		t.Fatalf("WaitForDeviceAuthorization: %v", err)
	}

	if tokenData.AccessToken != "tid=full-flow-copilot-token" {
		t.Errorf("AccessToken = %q, want copilot token", tokenData.AccessToken)
	}
	if tokenData.Source != "device_code_flow" {
		t.Errorf("Source = %q, want %q", tokenData.Source, "device_code_flow")
	}

	// Verify token was persisted
	loaded := ts.Get()
	if loaded == nil {
		t.Fatal("token not persisted")
	}
	if loaded.AccessToken != "tid=full-flow-copilot-token" {
		t.Errorf("persisted token = %q, want copilot token", loaded.AccessToken)
	}
}

func TestDeviceCodeFlow_InvalidClientID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"error":             "incorrect_client_credentials",
			"error_description": "Client ID not found.",
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithDeviceCodeURL(server.URL))

	_, err := handler.InitiateDeviceCode(context.Background())
	if err == nil {
		t.Fatal("expected error for invalid client ID")
	}
}

func TestDeviceCodeFlow_CopilotExchangeFailure(t *testing.T) {
	// GitHub token succeeds but Copilot exchange fails (e.g., no subscription)
	githubTokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "gho_test-token",
			"token_type":   "bearer",
		})
	}))
	defer githubTokenServer.Close()

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"message": "Not Found"})
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts,
		WithAccessTokenURL(githubTokenServer.URL),
		WithCopilotExchangerURL(copilotServer.URL),
	)

	// Test exchange after getting a GitHub token
	_, err := handler.exchangeAndValidate(context.Background(), "gho_test-token")
	if err == nil {
		t.Fatal("expected error when Copilot exchange fails")
	}
}

func TestDeviceCodeFlow_SubscriptionValidation(t *testing.T) {
	// Verify that after exchange, the token is tested by making a Copilot API call
	calledWithToken := ""
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledWithToken = r.Header.Get("Authorization")
		if r.URL.Path == "/copilot_internal/v2/token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"expires_at": time.Now().Add(30 * time.Minute).Unix(),
				"refresh_in": 1500,
				"token":      "tid=sub-valid-token",
			})
			return
		}
		// Any other path should not be called during device code flow
		t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts,
		WithCopilotExchangerURL(copilotServer.URL),
	)

	// The exchange step calls /copilot_internal/v2/token
	tokenData, err := handler.exchangeAndValidate(context.Background(), "gho_test-github-token")
	if err != nil {
		t.Fatalf("exchangeAndValidate: %v", err)
	}

	if calledWithToken != "Bearer gho_test-github-token" {
		t.Errorf("Authorization header = %q, want Bearer gho_test-github-token", calledWithToken)
	}
	if tokenData.AccessToken != "tid=sub-valid-token" {
		t.Errorf("token = %q, want copilot token", tokenData.AccessToken)
	}
}

func TestDeviceCodeFlow_ContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always return pending
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithAccessTokenURL(server.URL))

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := handler.pollForAccessToken(ctx, "DC-cancel-code")
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
}

func TestDeviceCodeFlow_ConcurrentInitiate(t *testing.T) {
	// Ensure multiple goroutines can safely initiate device code flows
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "DC-concurrent",
			UserCode:        "CONC-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithDeviceCodeURL(server.URL))

	const numGoroutines = 5
	results := make(chan *DeviceCodeResponse, numGoroutines)
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

func TestDeviceCodeFlow_GetCopilotToken_UsesCached(t *testing.T) {
	// When a valid token is already cached, GetCopilotToken should return it
	exchangeCount := 0
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"token":      "tid=fresh-token",
		})
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	ts.Save(&TokenData{
		AccessToken: "tid=cached-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute),
		ObtainedAt:  time.Now(),
		Source:      "device_code_flow",
	})

	handler := NewDeviceCodeHandler(ts, WithCopilotExchangerURL(copilotServer.URL))

	token, err := handler.GetCopilotToken(context.Background())
	if err != nil {
		t.Fatalf("GetCopilotToken: %v", err)
	}

	if token != "tid=cached-token" {
		t.Errorf("token = %q, want cached token", token)
	}

	if exchangeCount != 0 {
		t.Errorf("exchange was called %d times, want 0 (should use cache)", exchangeCount)
	}
}

func TestDeviceCodeFlow_GetCopilotToken_ExpiredRevalidates(t *testing.T) {
	// When the cached token is expired, GetCopilotToken should re-validate
	exchangeCount := 0
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchangeCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"expires_at": time.Now().Add(30 * time.Minute).Unix(),
			"refresh_in": 1500,
			"token":      fmt.Sprintf("tid=refreshed-token-%d", exchangeCount),
		})
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	// Save an expired token that includes a GitHub token for re-exchange
	ts.Save(&TokenData{
		AccessToken: "tid=expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		GitHubToken: "gho_stored-github-token",
	})

	handler := NewDeviceCodeHandler(ts, WithCopilotExchangerURL(copilotServer.URL))

	token, err := handler.GetCopilotToken(context.Background())
	if err != nil {
		t.Fatalf("GetCopilotToken: %v", err)
	}

	if token != "tid=refreshed-token-1" {
		t.Errorf("token = %q, want refreshed token", token)
	}

	if exchangeCount != 1 {
		t.Errorf("exchange was called %d times, want 1", exchangeCount)
	}
}

func TestDeviceCodeFlow_GetCopilotToken_ExpiredNoGitHubToken(t *testing.T) {
	// When the cached token is expired and there's no stored GitHub token,
	// GetCopilotToken should return an error
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected exchange call")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer copilotServer.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	// Save an expired token WITHOUT a GitHub token
	ts.Save(&TokenData{
		AccessToken: "tid=expired-token",
		ExpiresAt:   time.Now().Add(-1 * time.Hour),
		ObtainedAt:  time.Now().Add(-2 * time.Hour),
		Source:      "device_code_flow",
		// GitHubToken is empty
	})

	handler := NewDeviceCodeHandler(ts, WithCopilotExchangerURL(copilotServer.URL))

	_, err := handler.GetCopilotToken(context.Background())
	if err == nil {
		t.Fatal("expected error when no GitHub token available for re-exchange")
	}
}

func TestDeviceCodeFlow_DeviceCodeError_Type(t *testing.T) {
	err := &DeviceCodeError{Code: "expired_token", Description: "The code has expired."}
	if err.Error() != "device code error: expired_token: The code has expired." {
		t.Errorf("Error() = %q, unexpected", err.Error())
	}
}

func TestDeviceCodeFlow_PendingFlowTracking(t *testing.T) {
	// Verify that pending device flows are tracked and can be retrieved
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "DC-tracking",
			UserCode:        "TRAC-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	dir, _ := os.MkdirTemp("", "device-code-test-*")
	defer os.RemoveAll(dir)

	ts, _ := NewTokenStore(filepath.Join(dir, "token.json"))
	handler := NewDeviceCodeHandler(ts, WithDeviceCodeURL(server.URL))

	resp, err := handler.InitiateDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("InitiateDeviceCode: %v", err)
	}

	// Check that the pending flow is tracked
	pending := handler.GetPendingFlow(resp.DeviceCode)
	if pending == nil {
		t.Fatal("pending flow not tracked")
	}
	if pending.UserCode != "TRAC-1234" {
		t.Errorf("UserCode = %q, want %q", pending.UserCode, "TRAC-1234")
	}
	if pending.VerificationURI != "https://github.com/login/device" {
		t.Errorf("VerificationURI = %q, unexpected", pending.VerificationURI)
	}

	// Non-existent device code should return nil
	if handler.GetPendingFlow("nonexistent") != nil {
		t.Error("expected nil for non-existent device code")
	}
}
