package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type ZAIProvider struct{}

func (p *ZAIProvider) Name() string { return "zai" }

func (p *ZAIProvider) Fetch(ctx context.Context, apiKey string) (*Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.z.ai/api/user/balance", nil)
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
		return nil, fmt.Errorf("z.ai returned status %d", resp.StatusCode)
	}

	var body struct {
		Balance  float64 `json:"balance"`
		Currency string  `json:"currency"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}

	info := &Info{
		Balance:  body.Balance,
		Currency: body.Currency,
	}
	if info.Currency == "" {
		info.Currency = "CNY"
	}
	return info, nil
}
