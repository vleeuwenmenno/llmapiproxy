package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/menno/llmapiproxy/internal/backend"
)

const codexLoopbackListenAddr = ":1455"

type codexLoopbackHandler struct {
	registry   *backend.Registry
	appBaseURL string
}

func newCodexLoopbackHandler(registry *backend.Registry, appBaseURL string) http.Handler {
	h := &codexLoopbackHandler{
		registry:   registry,
		appBaseURL: strings.TrimRight(appBaseURL, "/"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", h.handleCallback)
	return mux
}

func (h *codexLoopbackHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	errParam := r.URL.Query().Get("error")
	if errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		log.Printf("codex loopback callback error: %s: %s", errParam, errDesc)
		http.Redirect(w, r, h.settingsURL("OAuth authentication failed: "+errParam), http.StatusSeeOther)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Redirect(w, r, h.settingsURL("OAuth callback failed: missing code or state parameter"), http.StatusSeeOther)
		return
	}

	backendName, err := h.registry.HandleCodexLoopbackCallback(r.Context(), code, state)
	if err != nil {
		log.Printf("codex loopback callback error: %v", err)
		http.Redirect(w, r, h.settingsURL("OAuth callback failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	log.Printf("oauth: successfully authenticated backend %s via loopback callback", backendName)
	http.Redirect(w, r, h.settingsURL(fmt.Sprintf("%s authentication successful!", backendName)), http.StatusSeeOther)
}

func (h *codexLoopbackHandler) settingsURL(msg string) string {
	return h.appBaseURL + "/ui/models?msg=" + url.QueryEscape(msg)
}
