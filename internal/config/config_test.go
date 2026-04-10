package config

import "testing"

func TestBackendConfigModels_StringForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - gpt-4o
      - glm-5.1
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" {
		t.Errorf("models[0].ID = %q, want gpt-4o", models[0].ID)
	}
	if models[0].ContextLength != nil {
		t.Errorf("models[0].ContextLength = %v, want nil", models[0].ContextLength)
	}
	if models[1].ID != "glm-5.1" {
		t.Errorf("models[1].ID = %q, want glm-5.1", models[1].ID)
	}
}

func TestBackendConfigModels_ObjectForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - id: glm-5.1
        context_length: 131072
        max_output_tokens: 8192
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	m := models[0]
	if m.ID != "glm-5.1" {
		t.Errorf("ID = %q, want glm-5.1", m.ID)
	}
	if m.ContextLength == nil || *m.ContextLength != 131072 {
		t.Errorf("ContextLength = %v, want 131072", m.ContextLength)
	}
	if m.MaxOutputTokens == nil || *m.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %v, want 8192", m.MaxOutputTokens)
	}
}

func TestBackendConfigModels_MixedForm(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
    models:
      - gpt-4o
      - id: glm-5.1
        context_length: 128000
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	models := cfg.Backends[0].Models
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4o" || models[0].ContextLength != nil {
		t.Errorf("unexpected models[0]: %+v", models[0])
	}
	if models[1].ID != "glm-5.1" || models[1].ContextLength == nil || *models[1].ContextLength != 128000 {
		t.Errorf("unexpected models[1]: %+v", models[1])
	}
}

func TestModelIDs(t *testing.T) {
	bc := BackendConfig{
		Models: []ModelConfig{{ID: "a"}, {ID: "b"}, {ID: "c"}},
	}
	ids := bc.ModelIDs()
	if len(ids) != 3 || ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Errorf("ModelIDs() = %v", ids)
	}
}
