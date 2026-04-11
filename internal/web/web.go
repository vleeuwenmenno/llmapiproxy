package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/quota"
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
	}).ParseFS(templateFS, "templates/*.html"))
}

type UI struct {
	cfgMgr    *config.Manager
	collector *stats.Collector
	registry  *backend.Registry
	store     *stats.Store
}

func NewUI(cfgMgr *config.Manager, collector *stats.Collector, registry *backend.Registry, store *stats.Store) *UI {
	return &UI{
		cfgMgr:    cfgMgr,
		collector: collector,
		registry:  registry,
		store:     store,
	}
}

// StaticFS returns the embedded static file system.
func StaticFS() embed.FS {
	return staticFS
}

const pageSize = 25

func (u *UI) Dashboard(w http.ResponseWriter, r *http.Request) {
	allTime := u.collector.Summarize(0)
	today := u.collector.Summarize(24 * time.Hour)
	hour := u.collector.Summarize(1 * time.Hour)
	recent, total := u.collector.RecentPaged(0, pageSize)

	data := map[string]any{
		"AllTime":    allTime,
		"Today":      today,
		"Hour":       hour,
		"Recent":     recent,
		"Backends":   u.registry.All(),
		"Page":       0,
		"TotalCount": total,
		"TotalPages": (total + pageSize - 1) / pageSize,
		"PageSize":   pageSize,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
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
	hour := u.collector.Summarize(1 * time.Hour)
	recent, total := u.collector.RecentPaged(page, pageSize)
	totalPages := (total + pageSize - 1) / pageSize
	if page >= totalPages && totalPages > 0 {
		page = totalPages - 1
		recent, total = u.collector.RecentPaged(page, pageSize)
	}

	data := map[string]any{
		"AllTime":    allTime,
		"Today":      today,
		"Hour":       hour,
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
	Name      string
	BaseURL   string
	Models    []string // prefixed with backend name for proxy use
	IsDynamic bool     // true when no explicit model list (accepts all)
	IconURL   string   // path to SVG icon, empty if unknown
	Enabled   bool     // false when backend is explicitly disabled
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
	ModelID  string
	Backends []string
}

func (u *UI) ModelsPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	modelBackends := make(map[string][]string) // bare model ID → backend names
	entries := make([]BackendEntry, 0, len(cfg.Backends))

	for _, bc := range cfg.Backends {
		isDynamic := len(bc.Models) == 0
		prefixed := make([]string, 0, len(bc.Models))
		seen := make(map[string]bool)
		for _, m := range bc.Models {
			prefixed = append(prefixed, bc.Name+"/"+m)
			if bc.IsEnabled() && !seen[m] {
				seen[m] = true
				modelBackends[m] = append(modelBackends[m], bc.Name)
			}
		}
		entries = append(entries, BackendEntry{
			Name:      bc.Name,
			BaseURL:   bc.BaseURL,
			Models:    prefixed,
			IsDynamic: isDynamic,
			IconURL:   iconForBackend(bc.Name),
			Enabled:   bc.IsEnabled(),
		})
	}

	var overlaps []OverlapEntry
	for modelID, backends := range modelBackends {
		if len(backends) >= 2 {
			overlaps = append(overlaps, OverlapEntry{ModelID: modelID, Backends: backends})
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

	// Find a sample model string (backend/model-id) for use in code examples.
	sampleModel := "backend/model-id"
	for _, e := range entries {
		if e.Enabled && len(e.Models) > 0 {
			sampleModel = e.Models[0]
			break
		}
	}

	// Build a map of model → configured backend priority for the routing dialog.
	routingByModel := make(map[string][]string)
	for _, mr := range cfg.Routing.Models {
		routingByModel[mr.Model] = mr.Backends
	}

	routingJSON, _ := json.Marshal(routingByModel)

	data := map[string]any{
		"Backends":    entries,
		"Overlaps":    overlaps,
		"DisplayAddr": displayAddr,
		"SampleModel": sampleModel,
		"Message":     r.URL.Query().Get("msg"),
		"RoutingJSON": template.JS(routingJSON),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "models.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// playgroundKeyEntry holds a label and the actual key value for the playground dropdown.
type playgroundKeyEntry struct {
	Label string
	Value string
}

// PlaygroundModels returns a JSON list of all models from enabled backends,
// prefixed with backend name. Used by the playground JS to populate the model combobox.
func (u *UI) PlaygroundModels(w http.ResponseWriter, r *http.Request) {
	var models []string
	for _, b := range u.registry.All() {
		list, err := b.ListModels(r.Context())
		if err != nil {
			log.Printf("playground: error listing models from %s: %v", b.Name(), err)
			continue
		}
		for _, m := range list {
			models = append(models, b.Name()+"/"+m.ID)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// PlaygroundPage renders the interactive model playground.
func (u *UI) PlaygroundPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Collect API keys: server-level keys and per-client keys.
	var keys []playgroundKeyEntry
	for i, k := range cfg.Server.APIKeys {
		keys = append(keys, playgroundKeyEntry{
			Label: fmt.Sprintf("Server key %d (%s)", i+1, maskKey(k)),
			Value: k,
		})
	}
	for _, c := range cfg.Clients {
		if c.APIKey != "" {
			keys = append(keys, playgroundKeyEntry{
				Label: fmt.Sprintf("%s (%s)", c.Name, maskKey(c.APIKey)),
				Value: c.APIKey,
			})
		}
	}

	// Collect all models from enabled backends (prefixed backend/model-id).
	var models []string
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		for _, m := range bc.Models {
			models = append(models, bc.Name+"/"+m)
		}
	}

	data := map[string]any{
		"APIKeys": keys,
		"Models":  models,
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "playground.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
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

	data := map[string]any{
		"LegacyKeys":    keys, // server.api_keys entries (unnamed, for migration notice only)
		"Backends":      backends,
		"StatsCount":    u.collector.TotalCount(),
		"Message":       msg,
		"IsError":       strings.HasPrefix(msg, "Error"),
		"DisableStats":  cfg.Server.DisableStats,
		"ConfigText":    configText,
		"Clients":       cfg.Clients,
		"ClientsJSON":   template.JS(func() []byte { b, _ := json.Marshal(cfg.Clients); return b }()),
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

type quotaResult struct {
	BackendName string
	BaseURL     string
	Info        *quota.Info
	Err         string
}

func (u *UI) QuotaFragment(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var mu sync.Mutex
	var results []quotaResult
	var wg sync.WaitGroup

	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		provider := quota.ForBackend(bc.BaseURL)
		if provider == nil {
			continue
		}
		localBC := bc
		localProvider := provider
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := localProvider.Fetch(ctx, localBC.APIKey)
			res := quotaResult{BackendName: localBC.Name, BaseURL: localBC.BaseURL}
			if err != nil {
				res.Err = err.Error()
			} else {
				res.Info = info
			}
			mu.Lock()
			results = append(results, res)
			mu.Unlock()
		}()
	}
	wg.Wait()

	sort.Slice(results, func(i, j int) bool {
		return results[i].BackendName < results[j].BackendName
	})

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "quota_fragment.html", results); err != nil {
		log.Printf("template error: %v", err)
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
