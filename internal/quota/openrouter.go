package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type OpenRouterProvider struct{}

func (p *OpenRouterProvider) Name() string { return "openrouter" }

func (p *OpenRouterProvider) Fetch(ctx context.Context, apiKey string) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openrouter.ai/api/v1/auth/key", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter returned status %d", resp.StatusCode)
	}

	var body struct {
		Data struct {
			Label      string   `json:"label"`
			Usage      float64  `json:"usage"`
			Limit      *float64 `json:"limit"`
			IsFreeTier bool     `json:"is_free_tier"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	info := &Info{
		Label:    body.Data.Label,
		Used:     body.Data.Usage,
		Currency: "USD",
	}
	if body.Data.Limit != nil {
		info.Limit = *body.Data.Limit
		info.Remaining = info.Limit - info.Used
		info.Balance = info.Remaining
	}
	return info, nil
}
