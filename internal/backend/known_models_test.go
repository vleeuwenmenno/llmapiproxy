package backend

import "testing"

func TestLookupKnownModel(t *testing.T) {
	tests := []struct {
		id      string
		wantCtx int64
		wantNil bool
	}{
		{"gpt-4o", 128000, false},
		{"gpt-4o-2024-08-06", 128000, false},
		{"gpt-4.1", 1047576, false},
		{"gpt-5.4", 1050000, false},
		{"gpt-5.4-2026-03-05", 1050000, false},
		{"gpt-5.4-pro", 1050000, false},
		{"gpt-5.4-mini", 400000, false},
		{"gpt-5.4-nano", 400000, false},
		{"gpt-5.3-codex", 400000, false},
		{"gpt-5.2-codex", 400000, false},
		{"gpt-5.1-codex", 400000, false},
		{"gpt-5.1-codex-max", 400000, false},
		{"gpt-5.1-codex-mini", 400000, false},
		{"gpt-5-codex", 400000, false},
		{"codex-mini-latest", 200000, false},
		{"gpt-5-mini", 400000, false},
		{"claude-3.5-sonnet-20241022", 200000, false},
		{"claude-sonnet-4-20250514", 1000000, false}, // Claude Sonnet 4.x has 1M ctx
		{"claude-opus-4-6", 1000000, false},
		{"claude-haiku-4-5-20251001", 200000, false},
		{"glm-5.1", 200000, false},
		{"glm-4.6v", 128000, false},
		{"minimax-m2.5", 204800, false},
		{"minimax-m2.5-free", 204800, false},
		{"kimi-k2.5", 262144, false},
		{"unknown-model-xyz-999", 0, true},
	}

	for _, tc := range tests {
		info := LookupKnownModel(tc.id)
		if tc.wantNil {
			if info != nil {
				t.Errorf("LookupKnownModel(%q) = %+v, want nil", tc.id, info)
			}
			continue
		}
		if info == nil {
			t.Errorf("LookupKnownModel(%q) = nil, want context_length=%d", tc.id, tc.wantCtx)
			continue
		}
		if info.ContextLength != tc.wantCtx {
			t.Errorf("LookupKnownModel(%q).ContextLength = %d, want %d", tc.id, info.ContextLength, tc.wantCtx)
		}
	}
}