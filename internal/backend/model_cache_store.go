package backend

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// modelCacheFile is the JSON format persisted to disk.
type modelCacheFile struct {
	Models    []Model  `json:"models"`
	ExpiresAt JSONTime `json:"expires_at"`
}

// JSONTime wraps time.Time for consistent RFC3339 JSON serialization.
type JSONTime struct {
	time.Time
}

func (t JSONTime) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.Time.Format(time.RFC3339))
}

func (t *JSONTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return fmt.Errorf("parsing time %q: %w", s, err)
	}
	t.Time = parsed
	return nil
}

// ModelCacheStore provides disk persistence for model caches.
// Each backend gets its own JSON file under baseDir.
// Thread-safe: concurrent Save/Load calls for different backends are parallel;
// same-backend calls are serialized per-key.
type ModelCacheStore struct {
	mu     sync.Mutex
	perKey map[string]*sync.Mutex
	baseDir string
}

// NewModelCacheStore creates a new store, creating baseDir if needed.
func NewModelCacheStore(baseDir string) (*ModelCacheStore, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("creating model cache dir %s: %w", baseDir, err)
	}
	return &ModelCacheStore{
		baseDir: baseDir,
		perKey:  make(map[string]*sync.Mutex),
	}, nil
}

func (s *ModelCacheStore) keyMu(backendName string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.perKey[backendName]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.perKey[backendName] = m
	return m
}

func (s *ModelCacheStore) filePath(backendName string) string {
	return filepath.Join(s.baseDir, backendName+"-models.json")
}

// Save persists models and their expiry time to disk for the given backend.
// Best-effort: errors are logged but not returned.
func (s *ModelCacheStore) Save(backendName string, models []Model, expiry time.Time) {
	mu := s.keyMu(backendName)
	mu.Lock()
	defer mu.Unlock()

	data := modelCacheFile{
		Models:    models,
		ExpiresAt: JSONTime{expiry},
	}

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to marshal model cache")
		return
	}

	path := s.filePath(backendName)
	dir := filepath.Dir(path)

	tmpFile, err := os.CreateTemp(dir, ".model-cache-*.tmp")
	if err != nil {
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to create temp file for model cache")
		return
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(raw); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to write model cache temp file")
		return
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to close model cache temp file")
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to rename model cache temp file")
		return
	}

	log.Debug().Str("backend", backendName).Int("models", len(models)).Msg("model cache saved to disk")
}

// Load reads models and their expiry time from disk for the given backend.
// Returns (models, expiry, true) on success, or (nil, zero, false) if no cache exists
// or the file is corrupt.
func (s *ModelCacheStore) Load(backendName string) ([]Model, time.Time, bool) {
	mu := s.keyMu(backendName)
	mu.Lock()
	defer mu.Unlock()

	path := s.filePath(backendName)
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn().Err(err).Str("backend", backendName).Msg("failed to read model cache file")
		}
		return nil, time.Time{}, false
	}

	var data modelCacheFile
	if err := json.Unmarshal(raw, &data); err != nil {
		log.Warn().Err(err).Str("backend", backendName).Msg("failed to parse model cache file")
		return nil, time.Time{}, false
	}

	log.Debug().Str("backend", backendName).Int("models", len(data.Models)).Msg("model cache loaded from disk")
	return data.Models, data.ExpiresAt.Time, true
}
