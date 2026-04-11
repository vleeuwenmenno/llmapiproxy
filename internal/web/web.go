package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/chat"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/stats"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var templates *template.Template

func init() {
	templates = template.Must(template.New("").Funcs(template.FuncMap{
		"maskKey": maskKey,
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"formatTime": func(t time.Time) string {
			return t.Format("15:04:05")
		},
		"formatDuration": func(ms int64) string {
			if ms < 1000 {
				return time.Duration(ms * int64(time.Millisecond)).String()
			}
			return time.Duration(ms * int64(time.Millisecond)).Round(time.Millisecond).String()
		},
		"formatTokenCount": func(n *int64) string {
			if n == nil {
				return ""
			}
			v := *n
			if v >= 1_000_000 {
				return fmt.Sprintf("%.1fM ", float64(v)/1_000_000)
			}
			if v >= 1000 {
				return fmt.Sprintf("%dK ", v/1000)
			}
			return fmt.Sprintf("%d ", v)
		},
		"gt": func(a, b int) bool {
			return a > b
		},
		"lt": func(a, b int) bool {
			return a < b
		},
		"add": func(a, b int) int {
			return a + b
		},
		"sub": func(a, b int) int {
			return a - b
		},
		"mul": func(a, b int) int {
			return a * b
		},
		"min": func(a, b int) int {
			if a < b {
				return a
			}
			return b
		},
		// routeChain renders an HTML snippet showing the attempted backend chain.
		// attempted is a comma-separated list; winner is the backend that succeeded.
		// If winner is empty (all failed), the last entry in attempted is shown as failed.
		"routeChain": func(attempted, winner string) template.HTML {
			if attempted == "" {
				return ""
			}
			parts := strings.Split(attempted, ",")
			var sb strings.Builder
			for i, name := range parts {
				if i > 0 {
					sb.WriteString(`<span style="color:var(--text-dim,#64748b);margin:0 0.3rem">→</span>`)
				}
				if name == winner && winner != "" {
					sb.WriteString(`<span style="color:var(--green,#34d399);font-family:monospace">✓ ` + name + `</span>`)
				} else {
					sb.WriteString(`<span style="color:var(--red,#f87171);font-family:monospace">✗ ` + name + `</span>`)
				}
			}
			return template.HTML(sb.String())
		},
	}).ParseFS(templateFS, "templates/*.html"))
}

type UI struct {
	cfgMgr    *config.Manager
	collector *stats.Collector
	registry  *backend.Registry
	store     *stats.Store
	chatStore *chat.ChatStore
}

func NewUI(cfgMgr *config.Manager, collector *stats.Collector, registry *backend.Registry, store *stats.Store, chatStore *chat.ChatStore) *UI {
	return &UI{
		cfgMgr:    cfgMgr,
		collector: collector,
		registry:  registry,
		store:     store,
		chatStore: chatStore,
	}
}

// StaticFS returns the embedded static file system.
func StaticFS() embed.FS {
	return staticFS
}

const pageSize = 25

func parseWindowParam(s string) (time.Duration, string) {
	switch s {
	case "2h":
		return 2 * time.Hour, "2h"
	case "3h":
		return 3 * time.Hour, "3h"
	case "6h":
		return 6 * time.Hour, "6h"
	case "12h":
		return 12 * time.Hour, "12h"
	default:
		return 1 * time.Hour, "1h"
	}
}

func (u *UI) Dashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "dashboard.html", nil); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// dashboardDataResponse is the JSON payload for the dashboard data endpoint.
type dashboardDataResponse struct {
	WindowLabel string        `json:"window_label"`
	WindowStats stats.Summary `json:"window_stats"`
	Today       stats.Summary `json:"today"`
	AllTime     stats.Summary `json:"all_time"`
	Backends    []dashBackend `json:"backends"`
	Clients     []dashClient  `json:"clients"`
	Recent      dashRecPage   `json:"recent"`
}

type dashBackend struct {
	Name     string `json:"name"`
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
	Errors   int    `json:"errors"`
}

type dashClient struct {
	Name     string `json:"name"`
	Requests int    `json:"requests"`
	Tokens   int    `json:"tokens"`
}

type dashRecPage struct {
	Items      []stats.Record `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	TotalPages int            `json:"total_pages"`
}

// DashboardData returns all dashboard data as a single JSON response.
// Supports ?window=1h|2h|3h|6h|12h and ?page=N.
func (u *UI) DashboardData(w http.ResponseWriter, r *http.Request) {
	windowDur, windowLabel := parseWindowParam(r.URL.Query().Get("window"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}

	windowStats := u.collector.Summarize(windowDur)
	today := u.collector.Summarize(24 * time.Hour)
	allTime := u.collector.Summarize(0)

	// Backends — window-scoped
	backends := make([]dashBackend, 0, len(windowStats.ByBackend))
	for name, count := range windowStats.ByBackend {
		backends = append(backends, dashBackend{
			Name:     name,
			Requests: count,
			Tokens:   windowStats.TokensByBackend[name],
			Errors:   windowStats.ErrorsByBackend[name],
		})
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].Requests > backends[j].Requests })

	// Clients — window-scoped
	clients := make([]dashClient, 0, len(windowStats.ByClient))
	for name, count := range windowStats.ByClient {
		clients = append(clients, dashClient{
			Name:     name,
			Requests: count,
			Tokens:   windowStats.TokensByClient[name],
		})
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Requests > clients[j].Requests })

	// Recent requests — window-scoped, paginated
	recent, total := u.collector.FilteredPaged(windowDur, page, pageSize)
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages > 0 && page >= totalPages {
		page = totalPages - 1
		recent, total = u.collector.FilteredPaged(windowDur, page, pageSize)
	}

	resp := dashboardDataResponse{
		WindowLabel: windowLabel,
		WindowStats: windowStats,
		Today:       today,
		AllTime:     allTime,
		Backends:    backends,
		Clients:     clients,
		Recent: dashRecPage{
			Items:      recent,
			Total:      total,
			Page:       page,
			TotalPages: totalPages,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("dashboard data encode error: %v", err)
	}
}

func (u *UI) StatsCards(w http.ResponseWriter, r *http.Request) {
	window, label := parseWindowParam(r.URL.Query().Get("window"))
	allTime := u.collector.Summarize(0)
	today := u.collector.Summarize(24 * time.Hour)
	windowStats := u.collector.Summarize(window)

	data := map[string]any{
		"AllTime":     allTime,
		"Today":       today,
		"WindowStats": windowStats,
		"WindowLabel": label,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "stats_cards_fragment.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (u *UI) StatsFragment(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}

	allTime := u.collector.Summarize(0)
	today := u.collector.Summarize(24 * time.Hour)
	recent, total := u.collector.RecentPaged(page, pageSize)
	totalPages := (total + pageSize - 1) / pageSize
	if page >= totalPages && totalPages > 0 {
		page = totalPages - 1
		recent, total = u.collector.RecentPaged(page, pageSize)
	}

	data := map[string]any{
		"AllTime":    allTime,
		"Today":      today,
		"Recent":     recent,
		"Backends":   u.registry.All(),
		"Page":       page,
		"TotalCount": total,
		"TotalPages": totalPages,
		"PageSize":   pageSize,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "stats_fragment.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (u *UI) ConfigPage(w http.ResponseWriter, r *http.Request) {
	configData, err := os.ReadFile(u.cfgMgr.Path())
	if err != nil {
		http.Error(w, "failed to read config", http.StatusInternalServerError)
		return
	}

	data := map[string]any{
		"Config":  string(configData),
		"Message": "",
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "config.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (u *UI) SaveConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := r.ParseForm(); err != nil {
		renderConfigMessage(w, "", "Failed to parse form: "+err.Error())
		return
	}

	redirectTo := r.FormValue("redirect")

	configText := r.FormValue("config")
	if configText == "" {
		if redirectTo != "" {
			http.Redirect(w, r, redirectTo+"?msg=Error:+Config+content+is+empty.", http.StatusSeeOther)
			return
		}
		renderConfigMessage(w, "", "Config content is empty")
		return
	}

	if err := u.cfgMgr.SaveRaw([]byte(configText)); err != nil {
		if redirectTo != "" {
			http.Redirect(w, r, redirectTo+"?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
			return
		}
		renderConfigMessage(w, configText, "Error: "+err.Error())
		return
	}

	u.registry.LoadFromConfig(u.cfgMgr.Get())
	if redirectTo != "" {
		http.Redirect(w, r, redirectTo+"?msg=Configuration+saved+and+reloaded+successfully!", http.StatusSeeOther)
		return
	}
	renderConfigMessage(w, configText, "Configuration saved and reloaded successfully!")
}

func renderConfigMessage(w http.ResponseWriter, configText string, message string) {
	data := map[string]any{
		"Config":  configText,
		"Message": message,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ExecuteTemplate(w, "config.html", data)
}

// BackendEntry holds display info for the models page.
type BackendEntry struct {
	Name        string
	BaseURL     string
	Models      []ModelEntry // enriched model metadata (nil for dynamic backends)
	IsDynamic   bool         // true when no explicit model list (accepts all)
	IconURL     string       // path to SVG icon, empty if unknown
	Enabled     bool         // false when backend is explicitly disabled
	StaticCount int          // pre-computed count for statically-configured backends
}

// ModelEntry holds display data for a single model in the UI.
type ModelEntry struct {
	FullID          string // backend/model-id
	BareID          string // model-id without backend prefix
	ContextLength   *int64
	MaxOutputTokens *int64
	Capabilities    []string
	DataSource      string // "upstream", "config", "builtin", or ""
}

// iconForBackend maps a backend name to a static icon URL.
func iconForBackend(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "openrouter"):
		return "/ui/static/icons/openrouter.svg"
	case strings.Contains(n, "zai") || strings.Contains(n, "z.ai"):
		return "/ui/static/icons/zai-white.svg"
	case strings.Contains(n, "openai"):
		return "/ui/static/icons/openai-white.svg"
	case strings.Contains(n, "claude") || strings.Contains(n, "anthropic"):
		return "/ui/static/icons/claude-white.svg"
	case strings.Contains(n, "ollama"):
		return "/ui/static/icons/ollama.svg"
	case strings.Contains(n, "zen") || strings.Contains(n, "opencode"):
		return "/ui/static/icons/openai-white.svg"
	default:
		return ""
	}
}

// OverlapEntry is a model ID that appears across multiple backends.
type OverlapEntry struct {
	ModelID       string            // canonical (last-path-segment) model ID
	Backends      []string          // backend names that have this model
	BackendModels map[string]string // backendName → actual model ID used by that backend
}

// lastPathSegment returns the last "/"-delimited segment of s.
// E.g. "z-ai/glm-5.1" → "glm-5.1", "glm-5.1" → "glm-5.1".
func lastPathSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func (u *UI) ModelsPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Build skeleton entries from config only — no network calls.
	// Model metadata is loaded lazily per-card by the browser via BackendModels.
	//
	// modelBackends: canonical model ID → backend names (for overlap detection).
	// backendModelIDs: canonical model ID → (backendName → actual model ID used).
	// Normalisation: the canonical key is the last path segment of the config model ID,
	// so "z-ai/glm-5.1" and "glm-5.1" both map to canonical key "glm-5.1".
	modelBackends := make(map[string][]string)
	backendModelIDs := make(map[string]map[string]string) // canonical → (backend → actual ID)
	entries := make([]BackendEntry, 0, len(cfg.Backends))

	for _, bc := range cfg.Backends {
		isDynamic := len(bc.Models) == 0

		// For static backends, build config-only entries (no live metadata yet).
		// These render immediately; JS will update them with metadata badges.
		var modelEntries []ModelEntry
		if !isDynamic {
			seenModelIDs := make(map[string]bool, len(bc.Models))
			for _, mc := range bc.Models {
				if seenModelIDs[mc.ID] {
					continue
				}
				seenModelIDs[mc.ID] = true
				modelEntries = append(modelEntries, ModelEntry{
					FullID: bc.Name + "/" + mc.ID,
					BareID: mc.ID,
				})
				if bc.IsEnabled() {
					canonical := lastPathSegment(mc.ID)
					modelBackends[canonical] = append(modelBackends[canonical], bc.Name)
					if backendModelIDs[canonical] == nil {
						backendModelIDs[canonical] = make(map[string]string)
					}
					backendModelIDs[canonical][bc.Name] = mc.ID
				}
			}
		}

		entries = append(entries, BackendEntry{
			Name:        bc.Name,
			BaseURL:     bc.BaseURL,
			Models:      modelEntries, // nil for dynamic; IDs-only for static
			IsDynamic:   isDynamic,
			IconURL:     iconForBackend(bc.Name),
			Enabled:     bc.IsEnabled(),
			StaticCount: len(modelEntries),
		})
	}

	// Compute overlaps from config only (no live fetch).
	var overlaps []OverlapEntry
	for canonicalID, backends := range modelBackends {
		if len(backends) >= 2 {
			overlaps = append(overlaps, OverlapEntry{
				ModelID:       canonicalID,
				Backends:      backends,
				BackendModels: backendModelIDs[canonicalID],
			})
		}
	}
	sort.Slice(overlaps, func(i, j int) bool { return overlaps[i].ModelID < overlaps[j].ModelID })

	// Derive a user-friendly listen address for the connect examples.
	listen := cfg.Server.Listen
	displayAddr := "localhost" + listen
	if strings.HasPrefix(listen, "0.0.0.0") {
		displayAddr = "localhost" + listen[len("0.0.0.0"):]
	} else if !strings.Contains(listen, ":") {
		displayAddr = listen
	}

	// Find a sample model string from config (no live fetch needed).
	sampleModel := "backend/model-id"
	for _, e := range entries {
		if e.Enabled && len(e.Models) > 0 {
			sampleModel = e.Models[0].FullID
			break
		}
	}

	// Build a map of model → configured backend priority for the routing dialog.
	type routingModelData struct {
		Backends []string `json:"backends"`
		Strategy string   `json:"strategy"`
	}
	routingByModel := make(map[string]routingModelData)
	for _, mr := range cfg.Routing.Models {
		routingByModel[mr.Model] = routingModelData{Backends: mr.Backends, Strategy: mr.Strategy}
	}

	routingJSON, _ := json.Marshal(routingByModel)

	// Build API key entries for the curl modal (masked for display, full for copy).
	apiKeyEntries := make([]keyEntry, len(cfg.Server.APIKeys))
	for i, k := range cfg.Server.APIKeys {
		apiKeyEntries[i] = keyEntry{Index: i, Masked: maskKey(k), Full: k}
	}

	// Collect all model IDs from enabled backends for the curl modal selector.
	var curlModels []string
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		for _, m := range bc.Models {
			curlModels = append(curlModels, bc.Name+"/"+m.ID)
		}
	}

	data := map[string]any{
		"Backends":       entries,
		"Overlaps":       overlaps,
		"DisplayAddr":    displayAddr,
		"SampleModel":    sampleModel,
		"Message":        r.URL.Query().Get("msg"),
		"RoutingJSON":    template.JS(routingJSON),
		"GlobalStrategy": cfg.Routing.Strategy,
		"ServerAPIKeys":  apiKeyEntries,
		"CurlModels":     curlModels,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "models.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// BackendModels returns a JSON array of ModelEntry for a single named backend.
// Called by the models page JS to lazy-load each backend card independently.
func (u *UI) BackendModels(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	cfg := u.cfgMgr.Get()

	var bc *config.BackendConfig
	for i := range cfg.Backends {
		if cfg.Backends[i].Name == name {
			bc = &cfg.Backends[i]
			break
		}
	}
	if bc == nil {
		http.NotFound(w, r)
		return
	}

	var b backend.Backend
	for _, bb := range u.registry.All() {
		if bb.Name() == name {
			b = bb
			break
		}
	}
	if b == nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	liveModels, _ := b.ListModels(ctx)
	liveByID := make(map[string]backend.Model, len(liveModels))
	for _, m := range liveModels {
		liveByID[m.ID] = m
	}

	isDynamic := len(bc.Models) == 0
	var entries []ModelEntry

	if isDynamic {
		for _, m := range liveModels {
			entries = append(entries, ModelEntry{
				FullID:          bc.Name + "/" + m.ID,
				BareID:          m.ID,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
				Capabilities:    m.Capabilities,
				DataSource:      "upstream",
			})
		}
	} else {
		for _, mc := range bc.Models {
			entry := ModelEntry{
				FullID: bc.Name + "/" + mc.ID,
				BareID: mc.ID,
			}
			if live, ok := liveByID[mc.ID]; ok {
				entry.ContextLength = live.ContextLength
				entry.MaxOutputTokens = live.MaxOutputTokens
				entry.Capabilities = live.Capabilities
				entry.DataSource = "upstream"
			}
			if mc.ContextLength != nil {
				entry.ContextLength = mc.ContextLength
				entry.DataSource = "config"
			}
			if mc.MaxOutputTokens != nil {
				entry.MaxOutputTokens = mc.MaxOutputTokens
				entry.DataSource = "config"
			}
			if entry.ContextLength == nil || entry.MaxOutputTokens == nil {
				if info := backend.LookupKnownModel(mc.ID); info != nil {
					if entry.ContextLength == nil {
						entry.ContextLength = &info.ContextLength
						if entry.DataSource == "" {
							entry.DataSource = "builtin"
						}
					}
					if entry.MaxOutputTokens == nil {
						entry.MaxOutputTokens = &info.MaxOutputTokens
						if entry.DataSource == "" {
							entry.DataSource = "builtin"
						}
					}
				}
			}
			entries = append(entries, entry)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		log.Printf("BackendModels: encode error: %v", err)
	}
}

// RefreshBackendModels clears the model cache for a specific backend and returns
// a fresh model list. Used by the UI refresh button on dynamic backends.
func (u *UI) RefreshBackendModels(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var b backend.Backend
	for _, bb := range u.registry.All() {
		if bb.Name() == name {
			b = bb
			break
		}
	}
	if b == nil {
		http.Error(w, "backend not found", http.StatusNotFound)
		return
	}

	// Clear the cache so next ListModels fetches fresh data.
	b.ClearModelCache()

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	models, err := b.ListModels(ctx)
	if err != nil {
		http.Error(w, "failed to list models: "+err.Error(), http.StatusBadGateway)
		return
	}

	type modelResp struct {
		ID              string   `json:"id"`
		DisplayName     string   `json:"display_name,omitempty"`
		ContextLength   *int64   `json:"context_length,omitempty"`
		MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
		Capabilities    []string `json:"capabilities,omitempty"`
	}

	resp := make([]modelResp, len(models))
	for i, m := range models {
		resp[i] = modelResp{
			ID:              name + "/" + m.ID,
			DisplayName:     m.DisplayName,
			ContextLength:   m.ContextLength,
			MaxOutputTokens: m.MaxOutputTokens,
			Capabilities:    m.Capabilities,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// playgroundModel is a compact model descriptor sent to the playground JS.
type playgroundModel struct {
	ID              string `json:"id"`
	ContextLength   *int64 `json:"context_length,omitempty"`
	MaxOutputTokens *int64 `json:"max_output_tokens,omitempty"`
}

// PlaygroundModels returns a JSON list of all models from enabled backends with metadata.
// Used by the playground JS to populate the model combobox.
func (u *UI) PlaygroundModels(w http.ResponseWriter, r *http.Request) {
	var models []playgroundModel
	for _, b := range u.registry.All() {
		list, err := b.ListModels(r.Context())
		if err != nil {
			log.Printf("playground: error listing models from %s: %v", b.Name(), err)
			continue
		}
		for _, m := range list {
			models = append(models, playgroundModel{
				ID:              b.Name() + "/" + m.ID,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// PlaygroundPage renders the interactive model playground.
func (u *UI) PlaygroundPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Find the internal playground client — its API key is embedded in the
	// page so the user never has to pick one manually.
	playgroundAPIKey := ""
	for _, c := range cfg.Clients {
		if c.Name == "playground" && c.APIKey != "" {
			playgroundAPIKey = c.APIKey
			break
		}
	}

	// Collect all models from enabled backends (prefixed backend/model-id).
	var models []playgroundModel
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		for _, m := range bc.Models {
			models = append(models, playgroundModel{ID: bc.Name + "/" + m.ID})
		}
	}

	data := map[string]any{
		"PlaygroundAPIKey": playgroundAPIKey,
		"Models":           models,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "playground.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// ── Chat API Handlers ──────────────────────────────────────────────────────

// ChatPage renders the interactive chat UI (sessions + messages).
func (u *UI) ChatPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Find the internal playground client — its API key is embedded in the
	// page so the user never has to pick one manually.
	playgroundAPIKey := ""
	for _, c := range cfg.Clients {
		if c.Name == "playground" && c.APIKey != "" {
			playgroundAPIKey = c.APIKey
			break
		}
	}

	// Collect all models from enabled backends (prefixed backend/model-id).
	var models []playgroundModel
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		for _, m := range bc.Models {
			models = append(models, playgroundModel{ID: bc.Name + "/" + m.ID})
		}
	}

	data := map[string]any{
		"ChatAPIKey":   playgroundAPIKey,
		"Models":       models,
		"TitleModel":   cfg.Server.TitleModel,
		"DefaultModel": cfg.Server.DefaultModel,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "chat.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// ChatModels returns a JSON list of all models from enabled backends with metadata.
func (u *UI) ChatModels(w http.ResponseWriter, r *http.Request) {
	var models []playgroundModel
	for _, b := range u.registry.All() {
		list, err := b.ListModels(r.Context())
		if err != nil {
			log.Printf("chat: error listing models from %s: %v", b.Name(), err)
			continue
		}
		for _, m := range list {
			models = append(models, playgroundModel{
				ID:              b.Name() + "/" + m.ID,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// ChatListSessions returns all chat sessions with aggregate stats, ordered by last message time.
func (u *UI) ChatListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := u.chatStore.ListSessionSummaries()
	if err != nil {
		http.Error(w, "failed to list sessions", http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []chat.SessionSummary{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

// ChatCreateSession creates a new chat session, optionally accepting a model from the request body.
func (u *UI) ChatCreateSession(w http.ResponseWriter, r *http.Request) {
	session, err := u.chatStore.CreateSession()
	if err != nil {
		http.Error(w, "failed to create session", http.StatusInternalServerError)
		return
	}

	// If the client sends a model, persist it immediately.
	var body struct {
		Model string `json:"model"`
	}
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Model != "" {
			_ = u.chatStore.UpdateSession(session.ID, session.Title, body.Model, session.SystemPrompt, session.Temperature, session.TopP, session.MaxTokens)
			session.Model = body.Model
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// ChatGetSession returns a single session with its messages.
func (u *UI) ChatGetSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	session, err := u.chatStore.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	messages, err := u.chatStore.ListMessages(id)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	if messages == nil {
		messages = []chat.Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"session":  session,
		"messages": messages,
	})
}

// ChatUpdateSession partially updates session fields.
// Only fields present in the JSON body are applied; omitted fields keep their current values.
func (u *UI) ChatUpdateSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Load current session to merge with partial update.
	current, err := u.chatStore.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}

	// Read the full body first so we can detect omitted fields via null pointers.
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

	// Merge: only override fields that were explicitly provided in the JSON.
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

	if err := u.chatStore.UpdateSession(id, title, model, sysPrompt, temp, topP, maxTok); err != nil {
		http.Error(w, "failed to update session", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ChatDeleteSession deletes a session and all its messages.
func (u *UI) ChatDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := u.chatStore.DeleteSession(id); err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ChatDeleteAllSessions deletes all chat sessions and their messages.
func (u *UI) ChatDeleteAllSessions(w http.ResponseWriter, r *http.Request) {
	if err := u.chatStore.DeleteAllSessions(); err != nil {
		http.Error(w, "failed to delete sessions", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ChatListMessages returns all messages for a session.
func (u *UI) ChatListMessages(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	messages, err := u.chatStore.ListMessages(id)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}
	if messages == nil {
		messages = []chat.Message{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(messages)
}

// ChatSaveMessage persists a single message to a session.
func (u *UI) ChatSaveMessage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req struct {
		Role         string  `json:"role"`
		Content      string  `json:"content"`
		Tokens       int     `json:"tokens"`
		PromptTokens int     `json:"prompt_tokens"`
		Model        string  `json:"model"`
		DurationMs   float64 `json:"duration_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	msg, err := u.chatStore.SaveMessage(id, req.Role, req.Content, req.Tokens, req.PromptTokens, req.Model, req.DurationMs)
	if err != nil {
		http.Error(w, "failed to save message", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msg)
}

// ChatGenerateTitle auto-generates a title for the session using an LLM.
func (u *UI) ChatGenerateTitle(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cfg := u.cfgMgr.Get()

	// Load the session and its messages.
	session, err := u.chatStore.GetSession(id)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	messages, err := u.chatStore.ListMessages(id)
	if err != nil {
		http.Error(w, "failed to load messages", http.StatusInternalServerError)
		return
	}

	// Build a condensed summary of the conversation for the title prompt.
	var conversation strings.Builder
	for _, m := range messages {
		role := m.Role
		if role == "system" {
			continue // skip system prompts for title generation
		}
		content := m.Content
		if len(content) > 200 {
			content = content[:200] + "..."
		}
		conversation.WriteString(fmt.Sprintf("%s: %s\n", role, content))
	}

	// Determine which model to use for title generation.
	titleModel := cfg.Server.TitleModel
	log.Printf("ChatGenerateTitle: session=%s config_title_model=%q session_model=%q", id, titleModel, session.Model)
	if titleModel == "" {
		// Fallback: use the session's model, or just truncate.
		titleModel = session.Model
		log.Printf("ChatGenerateTitle: config title_model empty, using session model=%q", titleModel)
	}
	if titleModel == "" {
		// Last resort: just use truncated first user message.
		log.Printf("ChatGenerateTitle: no model available, using fallback title")
		title := generateFallbackTitle(messages)
		if title == "" {
			title = "New Chat"
		}
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}

	// Find the backend that serves this model from config.
	var backendName, modelID string
	if parts := strings.SplitN(titleModel, "/", 2); len(parts) == 2 {
		backendName, modelID = parts[0], parts[1]
	} else {
		modelID = titleModel
	}
	log.Printf("ChatGenerateTitle: resolved backend=%q model=%q", backendName, modelID)

	var targetBackendCfg *config.BackendConfig
	for i := range cfg.Backends {
		bc := &cfg.Backends[i]
		if !bc.IsEnabled() {
			continue
		}
		if backendName != "" && bc.Name != backendName {
			continue
		}
		// Accept if: (a) backend has no models allowlist (accepts everything),
		// or (b) the model is explicitly listed, matching SupportsModel logic.
		if len(bc.Models) == 0 {
			targetBackendCfg = bc
			break
		}
		for _, m := range bc.Models {
			if m.ID == modelID {
				targetBackendCfg = bc
				break
			}
			if strings.HasSuffix(m.ID, "/*") {
				prefix := strings.TrimSuffix(m.ID, "/*")
				if strings.HasPrefix(modelID, prefix+"/") || modelID == prefix {
					targetBackendCfg = bc
					break
				}
			}
		}
		if targetBackendCfg != nil {
			break
		}
	}

	if targetBackendCfg == nil {
		log.Printf("ChatGenerateTitle: backend/model not found, using fallback title")
		title := generateFallbackTitle(messages)
		if title == "" {
			title = "New Chat"
		}
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}

	// Resolve the API key: check playground client's backend_keys first, then use backend's key.
	apiKey := targetBackendCfg.APIKey
	for _, c := range cfg.Clients {
		if c.Name == "playground" {
			if bk, ok := c.BackendKeys[targetBackendCfg.Name]; ok {
				apiKey = bk
			}
			break
		}
	}
	log.Printf("ChatGenerateTitle: calling LLM at %s with model=%s", targetBackendCfg.BaseURL, modelID)

	// Call the LLM to generate a title.
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
		title := generateFallbackTitle(messages)
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("ChatGenerateTitle: LLM call failed: %v", err)
		title := generateFallbackTitle(messages)
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("ChatGenerateTitle: LLM returned status %d: %s", resp.StatusCode, string(body))
		title := generateFallbackTitle(messages)
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
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
		log.Printf("ChatGenerateTitle: failed to decode LLM response: %v, choices=%d", err, len(titleResp.Choices))
		title := generateFallbackTitle(messages)
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}

	title := strings.TrimSpace(titleResp.Choices[0].Message.Content)
	title = strings.Trim(title, `"'`)
	if title == "" {
		title = generateFallbackTitle(messages)
	}
	if title == "" {
		title = "New Chat"
	}
	log.Printf("ChatGenerateTitle: generated title=%q", title)

	_ = u.chatStore.UpdateSessionTitle(id, title)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title": title})
}

// generateFallbackTitle creates a title from the first user message.
func generateFallbackTitle(messages []chat.Message) string {
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			title := m.Content
			if len(title) > 50 {
				title = title[:50] + "..."
			}
			title = strings.Split(title, "\n")[0] // first line only
			return title
		}
	}
	return ""
}

// ChatSetTitleModel updates the title_model config field and persists it.
func (u *UI) ChatSetTitleModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TitleModel string `json:"title_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := u.cfgMgr.UpdateTitleModel(req.TitleModel); err != nil {
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"title_model": req.TitleModel})
}

// ChatSetDefaultModel updates the default_model config field and persists it.
func (u *UI) ChatSetDefaultModel(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DefaultModel string `json:"default_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if err := u.cfgMgr.UpdateDefaultModel(req.DefaultModel); err != nil {
		http.Error(w, "failed to save: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"default_model": req.DefaultModel})
}

// keyEntry holds display data for a single API key on the settings page.
type keyEntry struct {
	Index  int
	Masked string
	Full   string
}

// backendSettingsEntry holds display data for a backend on the settings page.
type backendSettingsEntry struct {
	Name    string
	BaseURL string
	Enabled bool
}

// maskKey returns a masked version of an API key safe for display.
func maskKey(k string) string {
	if len(k) <= 8 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", len(k)-8) + k[len(k)-4:]
}

func (u *UI) SettingsPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()
	keys := make([]keyEntry, len(cfg.Server.APIKeys))
	for i, k := range cfg.Server.APIKeys {
		keys[i] = keyEntry{Index: i, Masked: maskKey(k), Full: k}
	}
	msg := r.URL.Query().Get("msg")

	// Load config file content for the raw config editor
	configData, err := os.ReadFile(u.cfgMgr.Path())
	configText := ""
	if err == nil {
		configText = string(configData)
	}

	// Build backend settings entries
	backends := make([]backendSettingsEntry, len(cfg.Backends))
	for i, b := range cfg.Backends {
		backends[i] = backendSettingsEntry{
			Name:    b.Name,
			BaseURL: b.BaseURL,
			Enabled: b.IsEnabled(),
		}
	}

	// Filter out the internal "playground" client — it is managed
	// automatically and should not be shown or deletable from the UI.
	visibleClients := make([]config.ClientConfig, 0, len(cfg.Clients))
	for _, c := range cfg.Clients {
		if c.Name != "playground" {
			visibleClients = append(visibleClients, c)
		}
	}

	data := map[string]any{
		"LegacyKeys":    keys, // server.api_keys entries (unnamed, for migration notice only)
		"Backends":      backends,
		"StatsCount":    u.collector.TotalCount(),
		"Message":       msg,
		"IsError":       strings.HasPrefix(msg, "Error"),
		"DisableStats":  cfg.Server.DisableStats,
		"ConfigText":    configText,
		"Clients":       visibleClients,
		"ClientsJSON":   template.JS(func() []byte { b, _ := json.Marshal(visibleClients); return b }()),
		"ServerHost":    cfg.Server.Host,
		"ServerPort":    cfg.Server.Port,
		"ModelCacheTTL": cfg.Server.ModelCacheTTL.String(),
		"OAuthStatuses": u.registry.OAuthStatuses(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "settings.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (u *UI) ClearStats(w http.ResponseWriter, r *http.Request) {
	u.collector.Clear()
	http.Redirect(w, r, "/ui/settings?msg=Stats+cleared+successfully.", http.StatusSeeOther)
}

func (u *UI) ToggleStats(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	enabled := r.FormValue("enabled") == "true"

	cfg := u.cfgMgr.Get()
	cfg.Server.DisableStats = !enabled

	data, err := yaml.Marshal(cfg)
	if err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	if err := u.cfgMgr.SaveRaw(data); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	if !enabled {
		// Clear in-memory stats when disabling
		u.collector.Clear()
	}

	status := "enabled"
	if !enabled {
		status = "disabled"
	}
	http.Redirect(w, r, "/ui/settings?msg=Stats+logging+"+status+".+Restart+the+proxy+for+changes+to+take+full+effect.", http.StatusSeeOther)
}

func (u *UI) AddAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	key := strings.TrimSpace(r.FormValue("key"))
	if key == "" {
		http.Redirect(w, r, "/ui/settings?msg=Error:+key+cannot+be+empty.", http.StatusSeeOther)
		return
	}
	cfg := u.cfgMgr.Get()
	newKeys := append(append([]string{}, cfg.Server.APIKeys...), key)
	if err := u.cfgMgr.UpdateAPIKeys(newKeys); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?msg=API+key+added.", http.StatusSeeOther)
}

func (u *UI) DeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	idx, err := strconv.Atoi(r.FormValue("index"))
	if err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+invalid+index.", http.StatusSeeOther)
		return
	}
	cfg := u.cfgMgr.Get()
	if idx < 0 || idx >= len(cfg.Server.APIKeys) {
		http.Redirect(w, r, "/ui/settings?msg=Error:+index+out+of+range.", http.StatusSeeOther)
		return
	}
	if len(cfg.Server.APIKeys) <= 1 && len(cfg.Clients) == 0 {
		http.Redirect(w, r, "/ui/settings?msg=Error:+cannot+remove+the+last+API+key.", http.StatusSeeOther)
		return
	}
	newKeys := append(append([]string{}, cfg.Server.APIKeys[:idx]...), cfg.Server.APIKeys[idx+1:]...)
	if err := u.cfgMgr.UpdateAPIKeys(newKeys); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?msg=API+key+deleted.", http.StatusSeeOther)
}

func (u *UI) RequestDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	if u.store == nil {
		http.Error(w, "store not available", http.StatusServiceUnavailable)
		return
	}
	rec, err := u.store.GetByID(id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "request_detail.html", rec); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (u *UI) AddClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	key := strings.TrimSpace(r.FormValue("key"))
	if name == "" || key == "" {
		http.Redirect(w, r, "/ui/settings?msg=Error:+name+and+key+are+required.", http.StatusSeeOther)
		return
	}
	cfg := u.cfgMgr.Get()
	newClients := append(append([]config.ClientConfig{}, cfg.Clients...), config.ClientConfig{Name: name, APIKey: key})
	if err := u.cfgMgr.UpdateClients(newClients); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?msg=Client+added.", http.StatusSeeOther)
}

func (u *UI) DeleteClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	name := r.FormValue("name")
	cfg := u.cfgMgr.Get()
	newClients := make([]config.ClientConfig, 0, len(cfg.Clients))
	for _, cl := range cfg.Clients {
		if cl.Name != name {
			newClients = append(newClients, cl)
		}
	}
	if err := u.cfgMgr.UpdateClients(newClients); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?msg=Client+deleted.", http.StatusSeeOther)
}

func (u *UI) UpdateServerAddr(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	host := strings.TrimSpace(r.FormValue("host"))
	portStr := strings.TrimSpace(r.FormValue("port"))
	port := 8080
	if portStr != "" {
		var err error
		port, err = strconv.Atoi(portStr)
		if err != nil || port < 1 || port > 65535 {
			http.Redirect(w, r, "/ui/settings?msg=Error:+port+must+be+a+number+between+1+and+65535.", http.StatusSeeOther)
			return
		}
	}
	if err := u.cfgMgr.UpdateServerAddr(host, port); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/ui/settings?msg=Server+address+updated.+Restart+the+proxy+for+changes+to+take+effect.", http.StatusSeeOther)
}

func (u *UI) UpdateModelCacheTTL(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	ttlStr := strings.TrimSpace(r.FormValue("ttl"))
	ttl, err := time.ParseDuration(ttlStr)
	if err != nil || ttl < 0 {
		http.Redirect(w, r, "/ui/settings?msg=Error:+invalid+duration.+Use+e.g.+5m,+30s,+1h,+or+0s+to+disable.", http.StatusSeeOther)
		return
	}
	if err := u.cfgMgr.UpdateModelCacheTTL(ttl); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	msg := "Model+cache+TTL+updated+to+" + ttlStr
	if ttl == 0 {
		msg = "Model+cache+disabled+(TTL+set+to+0)"
	}
	http.Redirect(w, r, "/ui/settings?msg="+msg, http.StatusSeeOther)
}

func (u *UI) SaveRouting(w http.ResponseWriter, r *http.Request) {
	var routing config.RoutingConfig
	if err := json.NewDecoder(r.Body).Decode(&routing); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := u.cfgMgr.SaveRouting(routing); err != nil {
		http.Error(w, "failed to save routing: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (u *UI) ToggleBackend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/settings?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	name := r.FormValue("name")
	enabled := r.FormValue("enabled") == "true"

	cfg := u.cfgMgr.Get()
	// Check that at least one enabled backend would remain
	if !enabled {
		enabledCount := 0
		for _, b := range cfg.Backends {
			if b.IsEnabled() {
				enabledCount++
			}
		}
		if enabledCount <= 1 {
			http.Redirect(w, r, "/ui/settings?msg=Error:+cannot+disable+the+last+enabled+backend.", http.StatusSeeOther)
			return
		}
	}

	redirectTo := r.FormValue("redirect")
	if redirectTo == "" {
		redirectTo = "/ui/settings"
	}

	if err := u.cfgMgr.ToggleBackend(name, enabled); err != nil {
		http.Redirect(w, r, redirectTo+"?msg=Error:"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	status := "disabled"
	if enabled {
		status = "enabled"
	}
	http.Redirect(w, r, redirectTo+"?msg=Backend+"+name+"+"+status+".", http.StatusSeeOther)
}

// --- OAuth management handlers ---

// OAuthStatus returns the authentication status for all OAuth backends as an HTMX fragment.
// This endpoint is called via HTMX to display live auth status on the settings page.
func (u *UI) OAuthStatus(w http.ResponseWriter, r *http.Request) {
	statuses := u.registry.OAuthStatuses()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "oauth_status.html", statuses); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// parseStatsFilter builds a StatsFilter from common query parameters.
// Supported params: window (e.g. "1h","24h","7d","30d"), from, to (ISO 8601),
// backend, model, client, errors ("1" for errors-only).
func parseStatsFilter(r *http.Request) stats.StatsFilter {
	q := r.URL.Query()
	var f stats.StatsFilter

	// Named window takes priority over explicit from/to
	if w := q.Get("window"); w != "" {
		var dur time.Duration
		switch w {
		case "1h":
			dur = time.Hour
		case "6h":
			dur = 6 * time.Hour
		case "24h":
			dur = 24 * time.Hour
		case "7d":
			dur = 7 * 24 * time.Hour
		case "30d":
			dur = 30 * 24 * time.Hour
		}
		if dur > 0 {
			f.From = time.Now().Add(-dur)
		}
	} else {
		if s := q.Get("from"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				f.From = t
			}
		}
		if s := q.Get("to"); s != "" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				f.To = t
			}
		}
	}

	f.Backend = q.Get("backend")
	f.Model = q.Get("model")
	f.Client = q.Get("client")
	f.ErrOnly = q.Get("errors") == "1"
	return f
}

// bucketSecsForFilter returns a reasonable time-series bucket width in seconds.
func bucketSecsForFilter(f stats.StatsFilter) int64 {
	if f.From.IsZero() {
		return 3600 // default 1-hour buckets
	}
	span := time.Since(f.From)
	switch {
	case span <= 2*time.Hour:
		return 60 // 1-min buckets
	case span <= 24*time.Hour:
		return 900 // 15-min buckets
	case span <= 7*24*time.Hour:
		return 3600 // 1-hour buckets
	default:
		return 4 * 3600 // 4-hour buckets
	}
}

// AnalyticsPage renders the analytics shell with filter dropdowns pre-populated.
func (u *UI) AnalyticsPage(w http.ResponseWriter, r *http.Request) {
	backends, _ := u.store.DistinctValues("backend")
	models, _ := u.store.DistinctValues("model")
	clients, _ := u.store.DistinctValues("client")

	data := map[string]any{
		"Backends": backends,
		"Models":   models,
		"Clients":  clients,
		// Pass current filter state for pre-selecting dropdowns
		"Filter": r.URL.Query(),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "analytics.html", data); err != nil {
		log.Printf("analytics template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// analyticsDataResponse is the JSON envelope returned by AnalyticsData.
type analyticsDataResponse struct {
	Summary     stats.Summary     `json:"summary"`
	Percentiles stats.Percentiles `json:"percentiles"`
	TimeSeries  []stats.TimePoint `json:"time_series"`
	TopModels   []stats.RankRow   `json:"top_models"`
	TopBackends []stats.RankRow   `json:"top_backends"`
	TopClients  []stats.RankRow   `json:"top_clients"`
	Records     analyticsRecPage  `json:"records"`
}

type analyticsRecPage struct {
	Items      []stats.Record `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	TotalPages int            `json:"total_pages"`
}

// AnalyticsData returns all filtered analytics data as a single JSON response.
func (u *UI) AnalyticsData(w http.ResponseWriter, r *http.Request) {
	f := parseStatsFilter(r)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}

	summary, err := u.store.FilteredSummary(f)
	if err != nil {
		log.Printf("analytics: summary error: %v", err)
	}
	pcts, err := u.store.FilteredPercentiles(f)
	if err != nil {
		log.Printf("analytics: percentiles error: %v", err)
	}
	ts, err := u.store.TimeSeries(f, bucketSecsForFilter(f))
	if err != nil {
		log.Printf("analytics: timeseries error: %v", err)
	}
	topModels, err := u.store.RankBy(f, "model", 20)
	if err != nil {
		log.Printf("analytics: rank models error: %v", err)
	}
	topBackends, err := u.store.RankBy(f, "backend", 20)
	if err != nil {
		log.Printf("analytics: rank backends error: %v", err)
	}
	topClients, err := u.store.RankBy(f, "client", 20)
	if err != nil {
		log.Printf("analytics: rank clients error: %v", err)
	}
	records, total, err := u.store.FilteredRecords(f, page, pageSize)
	if err != nil {
		log.Printf("analytics: records error: %v", err)
	}
	totalPages := (total + pageSize - 1) / pageSize

	resp := analyticsDataResponse{
		Summary:     summary,
		Percentiles: pcts,
		TimeSeries:  ts,
		TopModels:   topModels,
		TopBackends: topBackends,
		TopClients:  topClients,
		Records: analyticsRecPage{
			Items:      records,
			Total:      total,
			Page:       page,
			TotalPages: totalPages,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("analytics: json encode error: %v", err)
	}
}

// RoutingPage renders the routing analytics page.
func (u *UI) RoutingPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()
	backendNames := make([]string, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		if bc.IsEnabled() {
			backendNames = append(backendNames, bc.Name)
		}
	}
	routingJSON, _ := json.Marshal(cfg.Routing)
	data := map[string]any{
		"Routing":     cfg.Routing,
		"Backends":    backendNames,
		"RoutingJSON": template.JS(routingJSON),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "routing.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// OAuthCheckStatus proactively checks and refreshes the OAuth status for a
// specific backend. Unlike OAuthStatus which only reads cached state, this
// handler triggers a token refresh/re-exchange for backends that implement
// OAuthStatusRefresher (e.g., Copilot). Returns an HTMX fragment with the
// updated status for all backends.
func (u *UI) OAuthCheckStatus(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		http.Error(w, fmt.Sprintf("backend %q not found", backendName), http.StatusNotFound)
		return
	}

	// If the backend supports proactive refresh, trigger it.
	if refresher, ok := b.(backend.OAuthStatusRefresher); ok {
		if err := refresher.RefreshOAuthStatus(r.Context()); err != nil {
			log.Printf("oauth check status: refresh failed for %s: %v", backendName, err)
			// Don't return an error — still render the current status.
			// The status card will show the error state.
		} else {
			log.Printf("oauth check status: refresh succeeded for %s", backendName)
		}
	}

	// Return the full OAuth status fragment (HTMX will swap it in).
	statuses := u.registry.OAuthStatuses()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "oauth_status.html", statuses); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// OAuthLogin initiates the OAuth login flow for the specified backend.
// For Copilot, this initiates a device code flow and renders a page showing
// the user code and verification URL. For Codex, the user is redirected
// to the OpenAI authorization URL.
func (u *UI) OAuthLogin(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		http.Error(w, fmt.Sprintf("backend %q not found", backendName), http.StatusNotFound)
		return
	}

	loginHandler, ok := b.(backend.OAuthLoginHandler)
	if !ok {
		http.Error(w, fmt.Sprintf("backend %q does not support OAuth login", backendName), http.StatusBadRequest)
		return
	}

	authURL, state, err := loginHandler.InitiateLogin()
	if err != nil {
		log.Printf("oauth login error for %s: %v", backendName, err)
		http.Error(w, "failed to initiate login: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("oauth: initiated login for backend %s, state=%s", backendName, state)

	// Check if this is a device code flow (Copilot) or redirect flow (Codex).
	// For Copilot, authURL is JSON containing device code info.
	// For Codex, authURL is a URL to redirect to.
	var deviceCodeInfo backend.DeviceCodeLoginInfo
	if json.Unmarshal([]byte(authURL), &deviceCodeInfo) == nil && deviceCodeInfo.UserCode != "" {
		// Device code flow: render the device code page.
		data := map[string]any{
			"BackendName":    backendName,
			"UserCode":       deviceCodeInfo.UserCode,
			"VerificationURI": deviceCodeInfo.VerificationURI,
			"ExpiresIn":      deviceCodeInfo.ExpiresIn,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "device_code.html", data); err != nil {
			log.Printf("template error: %v", err)
			http.Error(w, "template error", http.StatusInternalServerError)
		}
		return
	}

	// Redirect flow (Codex PKCE): redirect to the authorization URL.
	http.Redirect(w, r, authURL, http.StatusFound)
}

// OAuthCallback handles the OAuth callback for the specified backend.
// For Codex, it exchanges the authorization code for tokens.
func (u *UI) OAuthCallback(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	errParam := r.URL.Query().Get("error")

	if errParam != "" {
		errDesc := r.URL.Query().Get("error_description")
		log.Printf("oauth callback error for %s: %s: %s", backendName, errParam, errDesc)
		http.Redirect(w, r, "/ui/settings?msg=OAuth+authentication+failed:+"+errParam, http.StatusSeeOther)
		return
	}

	if code == "" || state == "" {
		http.Error(w, "missing code or state parameter", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		http.Error(w, fmt.Sprintf("backend %q not found", backendName), http.StatusNotFound)
		return
	}

	callbackHandler, ok := b.(backend.OAuthCallbackHandler)
	if !ok {
		http.Error(w, fmt.Sprintf("backend %q does not support OAuth callbacks", backendName), http.StatusBadRequest)
		return
	}

	if err := callbackHandler.HandleCallback(r.Context(), code, state); err != nil {
		log.Printf("oauth callback error for %s: %v", backendName, err)
		http.Redirect(w, r, "/ui/settings?msg=OAuth+callback+failed:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	log.Printf("oauth: successfully authenticated backend %s", backendName)
	http.Redirect(w, r, "/ui/settings?msg="+backendName+"+authentication+successful!", http.StatusSeeOther)
}

// OAuthDeviceLogin initiates the device code flow for the specified backend.
// This is an alternative to the browser-based PKCE flow, designed for headless/SSH
// environments. It displays a device code and verification URL for the user.
func (u *UI) OAuthDeviceLogin(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		http.Error(w, fmt.Sprintf("backend %q not found", backendName), http.StatusNotFound)
		return
	}

	deviceHandler, ok := b.(backend.OAuthDeviceCodeLoginHandler)
	if !ok {
		http.Error(w, fmt.Sprintf("backend %q does not support device code login", backendName), http.StatusBadRequest)
		return
	}

	authURL, state, err := deviceHandler.InitiateDeviceCodeLogin()
	if err != nil {
		log.Printf("oauth device code login error for %s: %v", backendName, err)
		http.Error(w, "failed to initiate device code login: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Printf("oauth: initiated device code login for backend %s, state=%s", backendName, state)

	// Parse the JSON response to check for device code info.
	var deviceCodeInfo backend.DeviceCodeLoginInfo
	if json.Unmarshal([]byte(authURL), &deviceCodeInfo) == nil && deviceCodeInfo.UserCode != "" {
		// Device code flow: render the device code page.
		data := map[string]any{
			"BackendName":     backendName,
			"UserCode":        deviceCodeInfo.UserCode,
			"VerificationURI": deviceCodeInfo.VerificationURI,
			"ExpiresIn":       deviceCodeInfo.ExpiresIn,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "device_code.html", data); err != nil {
			log.Printf("template error: %v", err)
			http.Error(w, "template error", http.StatusInternalServerError)
		}
		return
	}

	// Fallback: redirect to regular login if device code info is not available.
	http.Redirect(w, r, "/ui/oauth/login/"+backendName, http.StatusSeeOther)
}

// OAuthDisconnect clears stored tokens for the specified backend.
func (u *UI) OAuthDisconnect(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		http.Error(w, fmt.Sprintf("backend %q not found", backendName), http.StatusNotFound)
		return
	}

	disconnectHandler, ok := b.(backend.OAuthDisconnectHandler)
	if !ok {
		http.Error(w, fmt.Sprintf("backend %q does not support disconnect", backendName), http.StatusBadRequest)
		return
	}

	if err := disconnectHandler.Disconnect(); err != nil {
		log.Printf("oauth disconnect error for %s: %v", backendName, err)
		http.Redirect(w, r, "/ui/settings?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	log.Printf("oauth: disconnected backend %s", backendName)
	http.Redirect(w, r, "/ui/settings?msg="+backendName+"+disconnected+successfully.", http.StatusSeeOther)
}

// RoutingConfigJSON returns the current routing configuration as JSON (used by
// the models page modal to merge per-model changes without clobbering global state).
func (u *UI) RoutingConfigJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u.cfgMgr.Get().Routing)
}

type routingDataResponse struct {
	Models []stats.ModelRoutingStats `json:"models"`
	Window string                    `json:"window"`
}

// RoutingData returns per-model routing analytics as JSON.
func (u *UI) RoutingData(w http.ResponseWriter, r *http.Request) {
	windowParam := r.URL.Query().Get("window")

	windowLabels := map[string]time.Duration{
		"1h":  time.Hour,
		"6h":  6 * time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	windowLabel := windowParam
	if windowLabel == "" {
		windowLabel = "24h"
	}
	windowDur := windowLabels[windowLabel]

	f := stats.StatsFilter{}
	if windowDur > 0 {
		f.From = time.Now().Add(-windowDur)
	}

	models, err := u.store.RoutingStats(f)
	if err != nil {
		log.Printf("routing: stats query error: %v", err)
		http.Error(w, "query error", http.StatusInternalServerError)
		return
	}
	if models == nil {
		models = []stats.ModelRoutingStats{}
	}

	resp := routingDataResponse{
		Models: models,
		Window: windowLabel,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("routing: json encode error: %v", err)
	}
}

// RoutingBackendFallbacks returns recent requests where the named backend was attempted
// but failed (fell through to another backend). Used to power the routing drilldown modal.
// Query: ?name=<backend>&window=<1h|6h|24h|7d|30d>
func (u *UI) RoutingBackendFallbacks(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "name parameter required", http.StatusBadRequest)
		return
	}
	windowParam := r.URL.Query().Get("window")
	windowLabels := map[string]time.Duration{
		"1h":  time.Hour,
		"6h":  6 * time.Hour,
		"24h": 24 * time.Hour,
		"7d":  7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour,
	}
	windowDur := windowLabels[windowParam]

	f := stats.StatsFilter{}
	if windowDur > 0 {
		f.From = time.Now().Add(-windowDur)
	}

	records, err := u.store.FallbacksForBackend(name, f, 200)
	if err != nil {
		log.Printf("routing: fallbacks query error: %v", err)
		http.Error(w, "query error", http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []stats.Record{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"items": records}); err != nil {
		log.Printf("routing: fallbacks encode error: %v", err)
	}
}
