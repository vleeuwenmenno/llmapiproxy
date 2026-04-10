package quota

import "context"

type Info struct {
	Balance   float64
	Currency  string
	Limit     float64
	Used      float64
	Remaining float64
	Label     string
	Error     string
}

type Provider interface {
	Name() string
	Fetch(ctx context.Context, apiKey string) (*Info, error)
}
