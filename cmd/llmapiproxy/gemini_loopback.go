package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/menno/llmapiproxy/internal/backend"
	log "github.com/rs/zerolog/log"
)

const geminiLoopbackListenAddr = ":42857"

type geminiLoopbackHandler struct {
	registry   *backend.Registry
	appBaseURL string
}

func newGeminiLoopbackHandler(registry *backend.Registry, appBaseURL string) http.Handler {
	h := &geminiLoopbackHandler{
		registry:   registry,
		appBaseURL: strings.TrimRight(appBaseURL, "/"),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2callback", h.handleCallback)
	return mux
}

func (h *geminiLoopbackHandler) handleCallback(w http.ResponseWriter, r *http.Request) {
	errParam := r.URL.Query().Get("error")
	if errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		log.Error().Str("error", errParam).Str("description", errDesc).Msg("gemini loopback callback error")
		http.Redirect(w, r, h.settingsURL("OAuth authentication failed: "+errParam), http.StatusSeeOther)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Redirect(w, r, h.settingsURL("OAuth callback failed: missing code or state parameter"), http.StatusSeeOther)
		return
	}

	backendName, err := h.registry.HandleGeminiLoopbackCallback(r.Context(), code, state)
	if err != nil {
		log.Error().Err(err).Msg("gemini loopback callback error")
		http.Redirect(w, r, h.settingsURL("OAuth callback failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	log.Info().Str("backend", backendName).Msg("oauth: successfully authenticated backend via gemini loopback callback")
	http.Redirect(w, r, h.settingsURL(fmt.Sprintf("%s authentication successful!", backendName)), http.StatusSeeOther)
}

func (h *geminiLoopbackHandler) settingsURL(msg string) string {
	return h.appBaseURL + "/ui/models?msg=" + url.QueryEscape(msg)
}
