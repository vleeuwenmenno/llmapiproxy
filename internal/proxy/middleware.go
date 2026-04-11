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

// AuthMiddleware validates the Authorization: Bearer <key> header using config.Manager.
func AuthMiddleware(cfgMgr *config.Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				log.Warn().Str("remote", r.RemoteAddr).Str("path", r.URL.Path).Msg("auth failed: missing authorization header")
				http.Error(w, `{"error":{"message":"Missing Authorization header","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
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
