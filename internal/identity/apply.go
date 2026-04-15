package identity

import (
	"net/http"

	"github.com/google/uuid"
)

// ApplyProfile modifies the HTTP request to match the given identity profile.
// It sets the User-Agent header (if the profile defines one) and merges
// profile headers into the request.
//
// Header precedence (highest wins):
//  1. extraHeaders from config (applied AFTER this function by the caller)
//  2. identity profile headers (applied here)
//  3. backend default headers (already set before this function is called)
//
// The caller should call ApplyProfile AFTER setting backend defaults but
// BEFORE applying config extraHeaders.
func ApplyProfile(httpReq *http.Request, profile *Profile, model string) {
	if profile == nil || profile.ID == ProfileNoneID {
		return
	}

	vars := DefaultVars(model)

	// Set User-Agent if the profile defines one.
	if ua := profile.RenderUserAgent(vars); ua != "" {
		httpReq.Header.Set("User-Agent", ua)
	}

	// Merge profile headers. Header values may contain template variables
	// (e.g. SessionID), so render them.
	for k, v := range profile.Headers {
		rendered, err := renderTemplate(v, vars)
		if err != nil {
			rendered = v // fallback to raw value
		}
		httpReq.Header.Set(k, rendered)
	}

	// Some profiles benefit from a per-request ID header.
	// Only set X-Request-Id if the profile doesn't define its own request ID header
	// and the request doesn't already have one.
	if httpReq.Header.Get("X-Request-Id") == "" && httpReq.Header.Get("x-client-request-id") == "" {
		httpReq.Header.Set("X-Request-Id", uuid.New().String())
	}
}
