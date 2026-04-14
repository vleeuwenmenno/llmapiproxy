package backend

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestModelCacheStore_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	models := []Model{
		{ID: "gpt-4o", Object: "model", Created: 1234, OwnedBy: "openai"},
		{ID: "gpt-4o-mini", Object: "model", Created: 5678, OwnedBy: "openai"},
	}
	expiry := time.Now().Add(5 * time.Minute)

	store.Save("test-backend", models, expiry)

	loaded, loadedExpiry, ok := store.Load("test-backend")
	if !ok {
		t.Fatal("Load returned ok=false, expected true")
	}
	if len(loaded) != len(models) {
		t.Fatalf("loaded %d models, want %d", len(loaded), len(models))
	}
	for i, m := range loaded {
		if m.ID != models[i].ID {
			t.Errorf("model[%d].ID = %q, want %q", i, m.ID, models[i].ID)
		}
	}
	// Expiry should round-trip within 1 second (RFC3339 granularity).
	if loadedExpiry.Sub(expiry).Abs() > time.Second {
		t.Errorf("expiry = %v, want %v", loadedExpiry, expiry)
	}
}

func TestModelCacheStore_LoadNonexistent(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	_, _, ok := store.Load("nonexistent")
	if ok {
		t.Error("Load for nonexistent backend returned ok=true, want false")
	}
}

func TestModelCacheStore_SaveCreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "caches", "nested")
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Error("base dir not created")
	}

	models := []Model{{ID: "test"}}
	store.Save("back", models, time.Now().Add(time.Minute))

	if _, _, ok := store.Load("back"); !ok {
		t.Error("Load returned false after Save")
	}
}

func TestModelCacheStore_LoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	// Write a corrupt file.
	corruptPath := filepath.Join(dir, "bad-models.json")
	if err := os.WriteFile(corruptPath, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	_, _, ok := store.Load("bad")
	if ok {
		t.Error("Load for corrupt file returned ok=true, want false")
	}
}

func TestModelCacheStore_FileName(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	expected := filepath.Join(dir, "mybackend-models.json")
	if got := store.filePath("mybackend"); got != expected {
		t.Errorf("filePath = %q, want %q", got, expected)
	}
}

func TestModelCacheStore_PersistentAcrossInstances(t *testing.T) {
	dir := t.TempDir()

	// First instance saves.
	store1, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore 1: %v", err)
	}
	models := []Model{
		{ID: "claude-3.5", Object: "model", OwnedBy: "anthropic"},
	}
	expiry := time.Now().Add(10 * time.Minute)
	store1.Save("persist-test", models, expiry)

	// Second instance loads — simulates server restart.
	store2, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore 2: %v", err)
	}

	loaded, _, ok := store2.Load("persist-test")
	if !ok {
		t.Fatal("second instance Load returned ok=false")
	}
	if len(loaded) != 1 || loaded[0].ID != "claude-3.5" {
		t.Errorf("loaded = %v, want [{ID:claude-3.5}]", loaded)
	}
}

func TestModelCacheStore_ModelFieldsPreserved(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	ctxLen := int64(128000)
	maxOut := int64(16384)
	models := []Model{
		{
			ID:              "gpt-4o",
			Object:          "model",
			Created:         1700000000,
			OwnedBy:         "openai",
			DisplayName:     "GPT-4o",
			ContextLength:   &ctxLen,
			MaxOutputTokens: &maxOut,
			Capabilities:    []string{"vision", "tools"},
		},
	}
	expiry := time.Now().Add(5 * time.Minute)
	store.Save("fields-test", models, expiry)

	loaded, _, ok := store.Load("fields-test")
	if !ok {
		t.Fatal("Load returned ok=false")
	}

	m := loaded[0]
	if m.DisplayName != "GPT-4o" {
		t.Errorf("DisplayName = %q, want %q", m.DisplayName, "GPT-4o")
	}
	if m.ContextLength == nil || *m.ContextLength != ctxLen {
		t.Errorf("ContextLength = %v, want %d", m.ContextLength, ctxLen)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens != maxOut {
		t.Errorf("MaxOutputTokens = %v, want %d", m.MaxOutputTokens, maxOut)
	}
	if len(m.Capabilities) != 2 || m.Capabilities[0] != "vision" {
		t.Errorf("Capabilities = %v, want [vision tools]", m.Capabilities)
	}
}

func TestModelCacheStore_ZeroExpiry(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	models := []Model{{ID: "test"}}
	store.Save("zero-expiry", models, time.Time{})

	loaded, expiry, ok := store.Load("zero-expiry")
	if !ok {
		t.Fatal("Load returned ok=false")
	}
	if len(loaded) != 1 {
		t.Errorf("len(loaded) = %d, want 1", len(loaded))
	}
	if !expiry.IsZero() {
		t.Errorf("expiry = %v, want zero time", expiry)
	}
}

func TestModelCacheStore_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	store, err := NewModelCacheStore(dir)
	if err != nil {
		t.Fatalf("NewModelCacheStore: %v", err)
	}

	// Concurrent saves for different backends.
	done := make(chan bool)
	for i := 0; i < 10; i++ {
		go func(idx int) {
			name := string(rune('a' + idx))
			models := []Model{{ID: "model-" + name}}
			store.Save(name, models, time.Now().Add(time.Minute))
			done <- true
		}(i)
	}
	for i := 0; i < 10; i++ {
		<-done
	}

	// All backends should be readable.
	for i := 0; i < 10; i++ {
		name := string(rune('a' + i))
		loaded, _, ok := store.Load(name)
		if !ok {
			t.Errorf("Load(%q) returned ok=false", name)
			continue
		}
		if len(loaded) != 1 || loaded[0].ID != "model-"+name {
			t.Errorf("Load(%q) = %v, unexpected", name, loaded)
		}
	}
}
