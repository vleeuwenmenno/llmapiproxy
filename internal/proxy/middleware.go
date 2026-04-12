package proxy

import (
	"context"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/config"
)

type clientContextKey struct{}

// ClientFromContext retrieves the ClientConfig stored in the context by AuthMiddleware.
func ClientFromContext(ctx context.Context) *config.ClientConfig {
	cl, _ := ctx.Value(clientContextKey{}).(*config.ClientConfig)
	return cl
}

// AuthMiddleware validates either Authorization: Bearer <key> or x-api-key
// using config.Manager so OpenAI- and Anthropic-style clients can both talk
// to the proxy.
func AuthMiddleware(cfgMgr *config.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			token := ""
			if strings.HasPrefix(auth, "Bearer ") {
				token = strings.TrimPrefix(auth, "Bearer ")
			} else if apiKey := strings.TrimSpace(r.Header.Get("x-api-key")); apiKey != "" {
				token = apiKey
			}

			if token == "" {
				log.Warn().Str("remote", r.RemoteAddr).Str("path", r.URL.Path).Msg("auth failed: missing authorization header")
				http.Error(w, `{"error":{"message":"Missing Authorization or x-api-key header","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}

			cl := cfgMgr.Get().LookupClient(token)
			if cl == nil {
				log.Warn().Str("remote", r.RemoteAddr).Str("path", r.URL.Path).Msg("auth failed: invalid API key")
				http.Error(w, `{"error":{"message":"Invalid API key","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}

			ctx := context.WithValue(r.Context(), clientContextKey{}, cl)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
