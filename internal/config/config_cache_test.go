package config

import (
	"testing"
	"time"
)

func TestModelCacheTTL_Default(t *testing.T) {
	yaml := `
server:
  listen: ":8080"
  api_keys: ["k"]
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse error: %v", err)
	}
	// Default should be 5 minutes when not specified.
	if cfg.Server.ModelCacheTTL != 5*time.Minute {
		t.Errorf("ModelCacheTTL = %v, want %v", cfg.Server.ModelCacheTTL, 5*time.Minute)
	}
}

func TestModelCacheTTL_Explicit(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantTTL time.Duration
	}{
		{
			name: "30 seconds",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "30s"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
			wantTTL: 30 * time.Second,
		},
		{
			name: "10 minutes",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "10m"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
			wantTTL: 10 * time.Minute,
		},
		{
			name: "1 hour",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "1h"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
			wantTTL: 1 * time.Hour,
		},
		{
			name: "disabled with 0s",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "0s"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
			wantTTL: 0,
		},
		{
			name: "disabled with 0",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: 0
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
			wantTTL: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse error: %v", err)
			}
			if cfg.Server.ModelCacheTTL != tt.wantTTL {
				t.Errorf("ModelCacheTTL = %v, want %v", cfg.Server.ModelCacheTTL, tt.wantTTL)
			}
		})
	}
}

func TestModelCacheTTL_Invalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{
			name: "negative duration",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "-5m"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
		},
		{
			name: "invalid string",
			yaml: `
server:
  listen: ":8080"
  api_keys: ["k"]
  model_cache_ttl: "abc"
backends:
  - name: test
    type: openai
    base_url: https://example.com
    api_key: k
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
