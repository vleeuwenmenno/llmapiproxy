package quota

import "strings"

func ForBackend(baseURL string) Provider {
	lower := strings.ToLower(baseURL)
	if strings.Contains(lower, "openrouter.ai") {
		return &OpenRouterProvider{}
	}
	if strings.Contains(lower, "z.ai") {
		return &ZAIProvider{}
	}
	return nil
}
