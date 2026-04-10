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
		{"claude-3.5-sonnet-20241022", 200000, false},
		{"claude-sonnet-4-20250514", 200000, false},
		{"glm-5.1", 200000, false},
		{"glm-4.6v", 128000, false},
		{"minimax-m2.5", 1048576, false},
		{"minimax-m2.5-free", 1048576, false},
		{"kimi-k2.5", 131072, false},
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
