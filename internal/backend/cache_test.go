package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
)

// newTestServer creates an httptest.Server that serves the given models from /models.
// It returns the server and a pointer to an atomic counter that tracks upstream calls.
func newTestServer(models []Model) (*httptest.Server, *atomic.Int64) {
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		list := upstreamModelList{Object: "list", Data: make([]upstreamModel, len(models))}
		for i, m := range models {
			list.Data[i] = upstreamModel{
				ID:      m.ID,
				Object:  m.Object,
				Created: m.Created,
				OwnedBy: m.OwnedBy,
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(list)
	})
	ts := httptest.NewServer(mux)
	return ts, &calls
}

// errorServer creates a server that always returns 500 on /models.
func errorServer() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	})
	return httptest.NewServer(mux)
}

func TestListModels_CacheHit(t *testing.T) {
	testModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
		{ID: "gpt-4o-mini", Object: "model", OwnedBy: "openai"},
	}
	ts, calls := newTestServer(testModels)
	defer ts.Close()

	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 5*time.Minute)

	models1, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls after first fetch = %d, want 1", calls.Load())
	}

	models2, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls after cached fetch = %d, want 1 (should use cache)", calls.Load())
	}

	if len(models1) != len(models2) {
		t.Errorf("cached models count %d != original %d", len(models2), len(models1))
	}
	for i := range models1 {
		if models1[i].ID != models2[i].ID {
			t.Errorf("cached model[%d].ID = %q, want %q", i, models2[i].ID, models1[i].ID)
		}
	}
}

func TestListModels_CacheExpiry(t *testing.T) {
	testModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
	}
	ts, calls := newTestServer(testModels)
	defer ts.Close()

	// Very short TTL so cache expires immediately.
	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 1*time.Nanosecond)

	// First call populates cache.
	_, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}

	// Wait for cache to expire (1ns TTL, so even a brief sleep is overkill).
	time.Sleep(1 * time.Millisecond)

	// Second call should hit upstream again because cache expired.
	_, err = b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls after expiry = %d, want 2", calls.Load())
	}
}

func TestListModels_StaleWhileError(t *testing.T) {
	// Start with a working server to populate the cache.
	goodModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
	}
	ts, calls := newTestServer(goodModels)

	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 5*time.Minute)

	// First call populates cache.
	models1, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}

	// Replace server with one that returns errors and update base URL.
	ts.Close()
	errTS := errorServer()
	defer errTS.Close()
	b.baseURL = errTS.URL

	// Short TTL so cache is expired, but stale data should be returned.
	b.modelCacheTTL = 1 * time.Nanosecond
	time.Sleep(1 * time.Millisecond)

	// Second call should return stale cached data even though upstream fails.
	models2, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels with stale cache: %v", err)
	}
	if len(models2) != len(models1) {
		t.Errorf("stale models count %d != original %d", len(models2), len(models1))
	}
	if models2[0].ID != models1[0].ID {
		t.Errorf("stale model ID = %q, want %q", models2[0].ID, models1[0].ID)
	}
}

func TestListModels_StaleWhileError_NoCache(t *testing.T) {
	// If there's no cache at all, the error should propagate.
	errTS := errorServer()
	defer errTS.Close()

	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: errTS.URL,
		APIKey:  "test-key",
	}, 5*time.Minute)

	_, err := b.ListModels(context.Background())
	if err == nil {
		t.Error("expected error when upstream fails with no cache, got nil")
	}
}

func TestListModels_CacheDisabled(t *testing.T) {
	testModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
	}
	ts, calls := newTestServer(testModels)
	defer ts.Close()

	// TTL=0 disables caching.
	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 0)

	_, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	_, err = b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("second ListModels: %v", err)
	}

	// Both calls should hit upstream — no caching.
	if calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2 (caching disabled)", calls.Load())
	}
}

func TestClearModelCache(t *testing.T) {
	testModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
	}
	ts, calls := newTestServer(testModels)
	defer ts.Close()

	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 5*time.Minute)

	// First call populates cache.
	_, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}

	// Second call should use cache.
	_, err = b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("cached ListModels: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1 (cache hit)", calls.Load())
	}

	// Clear cache.
	b.ClearModelCache()

	// Next call should hit upstream again.
	_, err = b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("post-clear ListModels: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream calls after clear = %d, want 2", calls.Load())
	}
}

func TestClearModelCache_ThenUpstreamFails(t *testing.T) {
	testModels := []Model{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
	}
	ts, _ := newTestServer(testModels)

	b := NewOpenAI(config.BackendConfig{
		Name:    "test",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	}, 5*time.Minute)

	// Populate cache.
	_, err := b.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}

	// Close good server and replace with error server.
	ts.Close()
	errTS := errorServer()
	defer errTS.Close()
	b.baseURL = errTS.URL

	// Clear cache — stale data is gone.
	b.ClearModelCache()

	// Now upstream fails and there's no stale cache to fall back on.
	_, err = b.ListModels(context.Background())
	if err == nil {
		t.Error("expected error when upstream fails after cache clear, got nil")
	}
}
