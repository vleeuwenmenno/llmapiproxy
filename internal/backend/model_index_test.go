package backend

import (
	"context"
	"io"
	"testing"

	"github.com/menno/llmapiproxy/internal/config"
)

type mockBackend struct {
	name   string
	models []Model
}

func (m *mockBackend) Name() string                                                { return m.name }
func (m *mockBackend) ChatCompletion(_ context.Context, _ *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	return nil, nil
}
func (m *mockBackend) ChatCompletionStream(_ context.Context, _ *ChatCompletionRequest) (io.ReadCloser, error) {
	return nil, nil
}
func (m *mockBackend) ListModels(_ context.Context) ([]Model, error) { return m.models, nil }
func (m *mockBackend) SupportsModel(id string) bool {
	for _, m := range m.models {
		if m.ID == id {
			return true
		}
	}
	return false
}
func (m *mockBackend) ResolveModelID(id string) string { return id }
func (m *mockBackend) ClearModelCache()                {}

func int64Ptr(v int64) *int64 { return &v }

func TestModelIndex_Alias_Basic(t *testing.T) {
	idx := NewModelIndex(DefaultCanonicalizeRules())

	backends := map[string]Backend{
		"openrouter": &mockBackend{
			name: "openrouter",
			models: []Model{
				{ID: "glm-5.1-precision", DisplayName: "GLM 5.1 Precision"},
			},
		},
	}

	cfgs := []config.BackendConfig{
		{
			Name:    "openrouter",
			Type:    "openai",
			BaseURL: "https://openrouter.ai/api/v1",
			ModelAliases: map[string]string{
				"glm-5.1-precision": "glm-5.1",
			},
		},
	}

	idx.Build(context.Background(), backends, cfgs)

	models := idx.FlatModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].CanonicalID != "glm-5.1" {
		t.Errorf("CanonicalID = %q, want %q", models[0].CanonicalID, "glm-5.1")
	}
	if models[0].DisplayName != "GLM 5.1 Precision" {
		t.Errorf("DisplayName = %q, want %q", models[0].DisplayName, "GLM 5.1 Precision")
	}
	if len(models[0].Backends) != 1 {
		t.Fatalf("expected 1 backend ref, got %d", len(models[0].Backends))
	}
	if models[0].Backends[0].RawModelID != "glm-5.1-precision" {
		t.Errorf("RawModelID = %q, want %q", models[0].Backends[0].RawModelID, "glm-5.1-precision")
	}

	rawID, ok := idx.ResolveBackendModelID("glm-5.1", "openrouter")
	if !ok {
		t.Fatal("ResolveBackendModelID returned false")
	}
	if rawID != "glm-5.1-precision" {
		t.Errorf("ResolveBackendModelID = %q, want %q", rawID, "glm-5.1-precision")
	}

	collisions := idx.Collisions()
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions, got %d", len(collisions))
	}
}

func TestModelIndex_Alias_Collision(t *testing.T) {
	idx := NewModelIndex(DefaultCanonicalizeRules())

	backends := map[string]Backend{
		"openrouter": &mockBackend{
			name: "openrouter",
			models: []Model{
				{ID: "glm-5.1", DisplayName: "GLM 5.1"},
				{ID: "glm-5.1-precision", DisplayName: "GLM 5.1 Precision"},
			},
		},
	}

	cfgs := []config.BackendConfig{
		{
			Name:    "openrouter",
			Type:    "openai",
			BaseURL: "https://openrouter.ai/api/v1",
			ModelAliases: map[string]string{
				"glm-5.1-precision": "glm-5.1",
			},
		},
	}

	idx.Build(context.Background(), backends, cfgs)

	models := idx.FlatModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model (collision should skip aliased), got %d", len(models))
	}
	if models[0].CanonicalID != "glm-5.1" {
		t.Errorf("CanonicalID = %q, want %q", models[0].CanonicalID, "glm-5.1")
	}
	if models[0].Backends[0].RawModelID != "glm-5.1" {
		t.Errorf("RawModelID = %q, want %q", models[0].Backends[0].RawModelID, "glm-5.1")
	}

	collisions := idx.Collisions()
	if len(collisions) != 1 {
		t.Fatalf("expected 1 collision, got %d", len(collisions))
	}
	if collisions[0].RawModelID != "glm-5.1-precision" {
		t.Errorf("collision RawModelID = %q, want %q", collisions[0].RawModelID, "glm-5.1-precision")
	}
	if collisions[0].Alias != "glm-5.1" {
		t.Errorf("collision Alias = %q, want %q", collisions[0].Alias, "glm-5.1")
	}
	if collisions[0].CollidesWith != "glm-5.1" {
		t.Errorf("collision CollidesWith = %q, want %q", collisions[0].CollidesWith, "glm-5.1")
	}
}

func TestModelIndex_Alias_DisabledModel_NoCollision(t *testing.T) {
	idx := NewModelIndex(DefaultCanonicalizeRules())

	backends := map[string]Backend{
		"openrouter": &mockBackend{
			name: "openrouter",
			models: []Model{
				{ID: "glm-5.1", Disabled: true},
				{ID: "glm-5.1-precision", DisplayName: "GLM 5.1 Precision"},
			},
		},
	}

	cfgs := []config.BackendConfig{
		{
			Name:    "openrouter",
			Type:    "openai",
			BaseURL: "https://openrouter.ai/api/v1",
			ModelAliases: map[string]string{
				"glm-5.1-precision": "glm-5.1",
			},
		},
	}

	idx.Build(context.Background(), backends, cfgs)

	models := idx.FlatModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].CanonicalID != "glm-5.1" {
		t.Errorf("CanonicalID = %q, want %q", models[0].CanonicalID, "glm-5.1")
	}
	if models[0].Backends[0].RawModelID != "glm-5.1-precision" {
		t.Errorf("RawModelID = %q, want %q", models[0].Backends[0].RawModelID, "glm-5.1-precision")
	}

	collisions := idx.Collisions()
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions (disabled model excluded), got %d", len(collisions))
	}
}

func TestModelIndex_Alias_CrossBackendOverlap(t *testing.T) {
	idx := NewModelIndex(DefaultCanonicalizeRules())

	backends := map[string]Backend{
		"openrouter": &mockBackend{
			name: "openrouter",
			models: []Model{
				{ID: "glm-5.1-precision", DisplayName: "GLM 5.1 Precision"},
			},
		},
		"zai": &mockBackend{
			name: "zai",
			models: []Model{
				{ID: "glm-5.1", DisplayName: "GLM 5.1"},
			},
		},
	}

	cfgs := []config.BackendConfig{
		{
			Name:    "openrouter",
			Type:    "openai",
			BaseURL: "https://openrouter.ai/api/v1",
			ModelAliases: map[string]string{
				"glm-5.1-precision": "glm-5.1",
			},
		},
		{
			Name:    "zai",
			Type:    "openai",
			BaseURL: "https://z.ai/api/v1",
		},
	}

	idx.Build(context.Background(), backends, cfgs)

	models := idx.FlatModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model (overlap), got %d", len(models))
	}
	if models[0].CanonicalID != "glm-5.1" {
		t.Errorf("CanonicalID = %q, want %q", models[0].CanonicalID, "glm-5.1")
	}
	if len(models[0].Backends) != 2 {
		t.Fatalf("expected 2 backends (overlap), got %d", len(models[0].Backends))
	}

	overlaps := idx.Overlaps()
	if len(overlaps) != 1 {
		t.Fatalf("expected 1 overlap, got %d", len(overlaps))
	}

	collisions := idx.Collisions()
	if len(collisions) != 0 {
		t.Errorf("expected 0 collisions (cross-backend overlap is fine), got %d", len(collisions))
	}
}

func TestModelIndex_NoAliases_Unchanged(t *testing.T) {
	idx := NewModelIndex(DefaultCanonicalizeRules())

	backends := map[string]Backend{
		"openrouter": &mockBackend{
			name: "openrouter",
			models: []Model{
				{ID: "glm-5.1", DisplayName: "GLM 5.1"},
			},
		},
	}

	cfgs := []config.BackendConfig{
		{
			Name:    "openrouter",
			Type:    "openai",
			BaseURL: "https://openrouter.ai/api/v1",
		},
	}

	idx.Build(context.Background(), backends, cfgs)

	models := idx.FlatModels()
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].CanonicalID != "glm-5.1" {
		t.Errorf("CanonicalID = %q, want %q", models[0].CanonicalID, "glm-5.1")
	}
}
