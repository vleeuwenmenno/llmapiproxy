package users

import (
	"context"
	"net/http"
	"strings"
)

type contextKey struct{}

// UserFromContext retrieves the authenticated User from the request context.
func UserFromContext(ctx context.Context) *User {
	u, _ := ctx.Value(contextKey{}).(*User)
	return u
}

// AuthMiddleware returns a Chi-compatible middleware that protects web UI routes.
// It checks for a valid session cookie; if missing/invalid, it redirects to the login page.
// Exempted paths are handled by the caller (register them outside the middleware group).
func AuthMiddleware(userStore *UserStore, sessionSecret []byte) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(SessionCookieName())
			if err == nil && cookie.Value != "" {
				sess, err := ParseSessionToken(cookie.Value, sessionSecret)
				if err == nil {
					// Look up user to ensure they still exist
					users, dbErr := userStore.ListUsers()
					if dbErr == nil {
						for _, u := range users {
							if u.Username == sess.Username {
								ctx := context.WithValue(r.Context(), contextKey{}, &u)
								next.ServeHTTP(w, r.WithContext(ctx))
								return
							}
						}
					}
				}
			}

			// For HTMX/AJAX requests, return 401 instead of redirect
			if r.Header.Get("HX-Request") == "true" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Redirect to login, preserving the original path for post-login redirect
			dest := "/ui/login"
			if r.URL.Path != "/ui/" && r.URL.Path != "/ui" {
				dest += "?next=" + r.URL.Path
			}
			http.Redirect(w, r, dest, http.StatusFound)
		})
	}
}

// IsAuthExemptPath returns true for paths that should not require authentication
// (login, setup, static assets, logout).
func IsAuthExemptPath(path string) bool {
	if path == "/ui/login" || path == "/ui/login/" {
		return true
	}
	if path == "/ui/logout" || path == "/ui/logout/" {
		return true
	}
	if path == "/ui/setup" || path == "/ui/setup/" {
		return true
	}
	if strings.HasPrefix(path, "/ui/static/") {
		return true
	}
	return false
}

// SetSessionCookie sets the session cookie on the response.
func SetSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName(),
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionDuration.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearSessionCookie clears the session cookie on the response.
func ClearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}
