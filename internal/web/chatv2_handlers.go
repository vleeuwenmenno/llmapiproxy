package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/chatv2"
	"github.com/menno/llmapiproxy/internal/config"
)

// chatv2Templates holds the separate template set for the chatv2 page using [[ ]] delimiters.
var chatv2Templates *template.Template

func init() {
	// Parse the chatv2 template with alternative delimiters to avoid
	// conflicts with Alpine.js {{ }} syntax.
	chatv2Tmpl := template.New("chatv2").Delims("[[", "]]")
	chatv2Tmpl = template.Must(chatv2Tmpl.Funcs(template.FuncMap{
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
	}).ParseFS(templateFS, "templates/chatv2.html"))
	chatv2Templates = chatv2Tmpl
}

// ChatV2Handler holds dependencies for all /ui/chatv2/ routes.
type ChatV2Handler struct {
	store    *chatv2.Store
	cfgMgr   *config.Manager
	registry *backend.Registry
}

// NewChatV2Handler creates a new handler for chatv2 routes.
func NewChatV2Handler(store *chatv2.Store, cfgMgr *config.Manager, registry *backend.Registry) *ChatV2Handler {
	return &ChatV2Handler{
		store:    store,
		cfgMgr:   cfgMgr,
		registry: registry,
	}
}

// ── Page Handler ──────────────────────────────────────────────────────

// ChatV2Page renders the chat beta page using the separate [[ ]] template set.
func (h *ChatV2Handler) ChatV2Page(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgMgr.Get()

	// Find the API key for the playground.
	playgroundAPIKey := ""
	for _, c := range cfg.Clients {
		if c.Name == "playground" && c.APIKey != "" {
			playgroundAPIKey = c.APIKey
			break
		}
	}
	if playgroundAPIKey == "" && len(cfg.Server.APIKeys) > 0 {
		playgroundAPIKey = cfg.Server.APIKeys[0]
	}

	data := map[string]any{
		"ActivePage":   "chatv2",
		"ChatAPIKey":   playgroundAPIKey,
		"TitleModel":   cfg.Server.TitleModel,
		"DefaultModel": cfg.Server.DefaultModel,
	}
	injectAuth(r, data)

	var buf bytes.Buffer
	if err := chatv2Templates.ExecuteTemplate(&buf, "chatv2.html", data); err != nil {
		log.Error().Err(err).Msg("chatv2: template error")
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

// ── Models ────────────────────────────────────────────────────────────

// chatv2Model is the JSON response for the chatv2 models endpoint.
type chatv2Model struct {
	ID                    string `json:"id"`
	DisplayName           string `json:"display_name"`
	ContextLength         int64  `json:"context_length"`
	MaxOutputTokens       int64  `json:"max_output_tokens"`
	SupportsSampling      bool   `json:"supports_sampling"`
	Vision                bool   `json:"vision"`
	UseMaxCompletionToken bool   `json:"use_max_completion_tokens"`
}

// ChatV2Models returns a JSON array of all available models with capability info.
func (h *ChatV2Handler) ChatV2Models(w http.ResponseWriter, r *http.Request) {
	routing := h.cfgMgr.Get().Routing
	allModels := h.registry.FlatModelList(r.Context(), routing)

	models := make([]chatv2Model, 0, len(allModels))
	for _, m := range allModels {
		entry := chatv2Model{
			ID:              m.ID,
			DisplayName:     m.DisplayName,
			ContextLength:   0,
			MaxOutputTokens: 0,
		}

		if m.ContextLength != nil {
			entry.ContextLength = *m.ContextLength
		}
		if m.MaxOutputTokens != nil {
			entry.MaxOutputTokens = *m.MaxOutputTokens
		}

		// Look up capability info from known_models.
		info := backend.LookupKnownModel(m.ID)
		if info != nil {
			if entry.DisplayName == "" {
				entry.DisplayName = info.DisplayName
			}
			if entry.ContextLength == 0 {
				entry.ContextLength = info.ContextLength
			}
			if entry.MaxOutputTokens == 0 {
				entry.MaxOutputTokens = info.MaxOutputTokens
			}
			entry.SupportsSampling = info.SupportsSampling
			entry.Vision = info.Vision
			entry.UseMaxCompletionToken = info.UseMaxCompletionTokens
		}

		// Also check capabilities list from the model metadata.
		for _, cap := range m.Capabilities {
			if cap == "vision" {
				entry.Vision = true
			}
		}

		models = append(models, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// ── Sessions CRUD ─────────────────────────────────────────────────────

// ChatV2ListSessions returns all chatv2 sessions as JSON.
func (h *ChatV2Handler) ChatV2ListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.store.ListSessions()
	if err != nil {
		log.Error().Err(err).Msg("chatv2: list sessions error")
		http.Error(w, "failed to list sessions", http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []chatv2.SessionSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

// ChatV2CreateSession creates a new chatv2 session.
func (h *ChatV2Handler) ChatV2CreateSession(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgMgr.Get()
	defaultModel := cfg.Server.DefaultModel
	if defaultModel == "" {
		defaultModel = "gpt-4o"
	}

	// Parse optional request body for initial settings.
	var req struct {
		Model        string  `json:"model"`
		SystemPrompt string  `json:"system_prompt"`
		Temperature  float64 `json:"temperature"`
		TopP         float64 `json:"top_p"`
		MaxTokens    int     `json:"max_tokens"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}

	model := req.Model
	if model == "" {
		model = defaultModel
	}

	// Check if the model has per-model defaults.
	modelDefaults, _ := h.store.GetModelDefaults(model)
	temperature := modelDefaults.Temperature
	topP := modelDefaults.TopP
	maxTokens := modelDefaults.MaxTokens
	systemPrompt := modelDefaults.SystemPrompt

	// Override with request body values if provided.
	if req.Temperature != 0 {
		temperature = req.Temperature
	}
	if req.TopP != 0 {
		topP = req.TopP
	}
	if req.MaxTokens != 0 {
		maxTokens = req.MaxTokens
	}
	if req.SystemPrompt != "" {
		systemPrompt = req.SystemPrompt
	}

	session, err := h.store.CreateSession("", model, systemPrompt, temperature, topP, maxTokens)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: create session error")
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(session)
}

// ChatV2GetSession returns a single session with its messages.
func (h *ChatV2Handler) ChatV2GetSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	session, err := h.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	messages, err := h.store.ListMessages(id)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: list messages error")
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	if messages == nil {
		messages = []chatv2.Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session":  session,
		"messages": messages,
	})
}

// ChatV2UpdateSession updates session fields (partial update).
func (h *ChatV2Handler) ChatV2UpdateSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	current, err := h.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var req struct {
		Title        *string  `json:"title"`
		Model        *string  `json:"model"`
		SystemPrompt *string  `json:"system_prompt"`
		Temperature  *float64 `json:"temperature"`
		TopP         *float64 `json:"top_p"`
		MaxTokens    *int     `json:"max_tokens"`
	}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	title := current.Title
	if req.Title != nil {
		title = *req.Title
	}
	model := current.Model
	if req.Model != nil {
		model = *req.Model
	}
	sysPrompt := current.SystemPrompt
	if req.SystemPrompt != nil {
		sysPrompt = *req.SystemPrompt
	}
	temp := current.Temperature
	if req.Temperature != nil {
		temp = *req.Temperature
	}
	topP := current.TopP
	if req.TopP != nil {
		topP = *req.TopP
	}
	maxTok := current.MaxTokens
	if req.MaxTokens != nil {
		maxTok = *req.MaxTokens
	}

	if err := h.store.UpdateSession(id, title, model, sysPrompt, temp, topP, maxTok); err != nil {
		log.Error().Err(err).Msg("chatv2: update session error")
		http.Error(w, "failed to update session", http.StatusInternalServerError)
		return
	}

	// Return the updated session.
	updated, _ := h.store.GetSession(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}

// ChatV2DeleteSession deletes a session and its messages.
func (h *ChatV2Handler) ChatV2DeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.store.DeleteSession(id); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ChatV2DeleteAllSessions deletes all sessions and their messages.
func (h *ChatV2Handler) ChatV2DeleteAllSessions(w http.ResponseWriter, r *http.Request) {
	if err := h.store.DeleteAllSessions(); err != nil {
		log.Error().Err(err).Msg("chatv2: delete all sessions error")
		http.Error(w, "failed to delete sessions", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Messages ──────────────────────────────────────────────────────────

// ChatV2ListMessages returns all messages for a session.
func (h *ChatV2Handler) ChatV2ListMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	messages, err := h.store.ListMessages(id)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: list messages error")
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	if messages == nil {
		messages = []chatv2.Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

// ChatV2SaveMessage persists a message to a session.
func (h *ChatV2Handler) ChatV2SaveMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Role         string  `json:"role"`
		Content      string  `json:"content"`
		Tokens       int     `json:"tokens"`
		PromptTokens int     `json:"prompt_tokens"`
		Model        string  `json:"model"`
		DurationMs   float64 `json:"duration_ms"`
		TPS          float64 `json:"tps"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	msg, err := h.store.SaveMessage(id, req.Role, req.Content, req.Tokens, req.PromptTokens, req.Model, req.DurationMs, req.TPS)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: save message error")
		http.Error(w, "failed to save message", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(msg)
}

// ── Title Generation ──────────────────────────────────────────────────

// ChatV2GenerateTitle auto-generates a title for the session.
// Falls back to truncating the first user message if no LLM is available.
func (h *ChatV2Handler) ChatV2GenerateTitle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfg := h.cfgMgr.Get()

	session, err := h.store.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	messages, err := h.store.ListMessages(id)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: list messages for title error")
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}

	// Build a condensed summary of the conversation.
	var conversation strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		content := m.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		conversation.WriteString(fmt.Sprintf("%s: %s\n", m.Role, content))
	}

	// Determine which model to use for title generation.
	titleModel := cfg.Server.TitleModel
	if titleModel == "" {
		titleModel = session.Model
	}
	if titleModel == "" {
		// Last resort: fallback to truncated first user message.
		title := chatv2FallbackTitle(messages)
		if title == "" {
			title = "New Chat"
		}
		_ = h.store.UpdateSession(id, title, session.Model, session.SystemPrompt, session.Temperature, session.TopP, session.MaxTokens)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}

	// Resolve backend and model ID.
	var backendName, modelID string
	if parts := strings.SplitN(titleModel, "/", 2); len(parts) == 2 {
		backendName, modelID = parts[0], parts[1]
	} else {
		modelID = titleModel
	}

	var targetBackendCfg *config.BackendConfig
	for i := range cfg.Backends {
		bc := &cfg.Backends[i]
		if !bc.IsEnabled() {
			continue
		}
		if backendName != "" && bc.Name != backendName {
			continue
		}
		if len(bc.Models) == 0 {
			targetBackendCfg = bc
			break
		}
		for _, m := range bc.Models {
			if m.ID == modelID {
				targetBackendCfg = bc
				break
			}
		}
		if targetBackendCfg != nil {
			break
		}
	}

	if targetBackendCfg == nil {
		title := chatv2FallbackTitle(messages)
		if title == "" {
			title = "New Chat"
		}
		_ = h.store.UpdateSession(id, title, session.Model, session.SystemPrompt, session.Temperature, session.TopP, session.MaxTokens)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}

	// Resolve the API key.
	apiKey := targetBackendCfg.APIKey
	for _, c := range cfg.Clients {
		if c.Name == "playground" {
			if bk, ok := c.BackendKeys[targetBackendCfg.Name]; ok {
				apiKey = bk
			}
			break
		}
	}

	// Call the LLM.
	titleReq := map[string]any{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "system", "content": "You are a title generator. Generate a concise 3-5 word title for the following conversation. Reply with ONLY the title, nothing else. No quotes, no punctuation."},
			{"role": "user", "content": conversation.String()},
		},
		"max_tokens":  20,
		"temperature": 0.3,
	}
	reqBody, _ := json.Marshal(titleReq)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", targetBackendCfg.BaseURL+"/chat/completions", strings.NewReader(string(reqBody)))
	if err != nil {
		h.writeFallbackTitle(w, id, session, messages)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: title generation LLM call failed")
		h.writeFallbackTitle(w, id, session, messages)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Error().Int("status", resp.StatusCode).Str("body", string(body)).Msg("chatv2: title generation LLM error")
		h.writeFallbackTitle(w, id, session, messages)
		return
	}

	var titleResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&titleResp); err != nil || len(titleResp.Choices) == 0 {
		log.Error().Err(err).Int("choices", len(titleResp.Choices)).Msg("chatv2: failed to decode title response")
		h.writeFallbackTitle(w, id, session, messages)
		return
	}

	title := strings.TrimSpace(titleResp.Choices[0].Message.Content)
	title = strings.Trim(title, `"'`)
	if title == "" {
		h.writeFallbackTitle(w, id, session, messages)
		return
	}

	_ = h.store.UpdateSession(id, title, session.Model, session.SystemPrompt, session.Temperature, session.TopP, session.MaxTokens)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title})
}

// writeFallbackTitle generates a fallback title and writes it as JSON.
func (h *ChatV2Handler) writeFallbackTitle(w http.ResponseWriter, id string, session *chatv2.Session, messages []chatv2.Message) {
	title := chatv2FallbackTitle(messages)
	if title == "" {
		title = "New Chat"
	}
	_ = h.store.UpdateSession(id, title, session.Model, session.SystemPrompt, session.Temperature, session.TopP, session.MaxTokens)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title})
}

// chatv2FallbackTitle creates a title from the first user message.
func chatv2FallbackTitle(messages []chatv2.Message) string {
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			title := m.Content
			if len(title) > 50 {
				title = title[:50] + "..."
			}
			title = strings.Split(title, "\n")[0]
			return title
		}
	}
	return ""
}

// ── Search ────────────────────────────────────────────────────────────

// ChatV2SearchSessions searches sessions by title and message content.
func (h *ChatV2Handler) ChatV2SearchSessions(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if query == "" {
		// Return all sessions if no query provided.
		h.ChatV2ListSessions(w, r)
		return
	}
	results, err := h.store.SearchSessions(query)
	if err != nil {
		log.Error().Err(err).Msg("chatv2: search sessions error")
		http.Error(w, "failed to search sessions", http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []chatv2.SessionSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

// ── Export ─────────────────────────────────────────────────────────────

// ChatV2ExportSession exports a session in the requested format.
func (h *ChatV2Handler) ChatV2ExportSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	format := r.URL.Query().Get("format")

	switch format {
	case "json":
		jsonStr, err := h.store.ExportSessionJSON(id)
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=session-%s.json", id[:8]))
		w.Write([]byte(jsonStr))
	default:
		// Default to markdown.
		md, err := h.store.ExportSessionMarkdown(id)
		if err != nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=session-%s.md", id[:8]))
		w.Write([]byte(md))
	}
}

// ── Model Defaults ────────────────────────────────────────────────────

// ChatV2GetDefaults returns all stored model defaults.
func (h *ChatV2Handler) ChatV2GetDefaults(w http.ResponseWriter, r *http.Request) {
	defaults, err := h.store.ListAllModelDefaults()
	if err != nil {
		log.Error().Err(err).Msg("chatv2: list model defaults error")
		http.Error(w, "failed to list model defaults", http.StatusInternalServerError)
		return
	}
	if defaults == nil {
		defaults = []chatv2.ModelDefaults{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(defaults)
}

// ChatV2SetDefaults sets per-model defaults.
func (h *ChatV2Handler) ChatV2SetDefaults(w http.ResponseWriter, r *http.Request) {
	model := chi.URLParam(r, "model")
	if model == "" {
		http.Error(w, "model parameter is required", http.StatusBadRequest)
		return
	}

	var req struct {
		Temperature  *float64 `json:"temperature"`
		TopP         *float64 `json:"top_p"`
		MaxTokens    *int     `json:"max_tokens"`
		SystemPrompt *string  `json:"system_prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Load existing defaults or use sensible defaults.
	existing, _ := h.store.GetModelDefaults(model)
	temperature := existing.Temperature
	topP := existing.TopP
	maxTokens := existing.MaxTokens
	systemPrompt := existing.SystemPrompt

	if req.Temperature != nil {
		temperature = *req.Temperature
	}
	if req.TopP != nil {
		topP = *req.TopP
	}
	if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	if req.SystemPrompt != nil {
		systemPrompt = *req.SystemPrompt
	}

	if err := h.store.SetModelDefaults(model, temperature, topP, maxTokens, systemPrompt); err != nil {
		log.Error().Err(err).Msg("chatv2: set model defaults error")
		http.Error(w, "failed to save defaults", http.StatusInternalServerError)
		return
	}

	updated, _ := h.store.GetModelDefaults(model)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
}
