package backend

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/identity"
)

const kimiDefaultMaxTokens = 32000

// KimiBackend wraps OpenAIBackend with Kimi-specific request rewriting:
//   - Disables thinking by default (avoids reasoning_content validation errors)
//   - Sets default max_tokens to 32000 when not specified
type KimiBackend struct {
	*OpenAIBackend
}

func NewKimi(cfg config.BackendConfig, cacheTTL time.Duration, profile *identity.Profile) *KimiBackend {
	return &KimiBackend{
		OpenAIBackend: NewOpenAI(cfg, cacheTTL, profile),
	}
}

// patchBody injects Kimi-specific fields into the serialized request body.
func patchBody(data []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}

	if _, exists := m["thinking"]; !exists {
		disabled, _ := json.Marshal(map[string]string{"type": "disabled"})
		m["thinking"] = disabled
	}

	if _, hasMaxTokens := m["max_tokens"]; !hasMaxTokens {
		if _, hasMaxCompletion := m["max_completion_tokens"]; !hasMaxCompletion {
			mt, _ := json.Marshal(kimiDefaultMaxTokens)
			m["max_tokens"] = mt
		}
	}

	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}

func (b *KimiBackend) ChatCompletion(ctx context.Context, req *ChatCompletionRequest) (*ChatCompletionResponse, error) {
	if len(req.RawBody) > 0 {
		req.RawBody = patchBody(req.RawBody)
	}
	return b.OpenAIBackend.ChatCompletion(ctx, req)
}

func (b *KimiBackend) ChatCompletionStream(ctx context.Context, req *ChatCompletionRequest) (io.ReadCloser, error) {
	if len(req.RawBody) > 0 {
		req.RawBody = patchBody(req.RawBody)
	}
	return b.OpenAIBackend.ChatCompletionStream(ctx, req)
}
