package proxy

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// AuthMiddleware validates the Authorization: Bearer <key> header.
func AuthMiddleware(validKeys []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				http.Error(w, `{"error":{"message":"Missing Authorization header","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}

			token := strings.TrimPrefix(auth, "Bearer ")
			valid := false
			for _, key := range validKeys {
				if subtle.ConstantTimeCompare([]byte(token), []byte(key)) == 1 {
					valid = true
					break
				}
			}

			if !valid {
				http.Error(w, `{"error":{"message":"Invalid API key","type":"auth_error"}}`, http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
