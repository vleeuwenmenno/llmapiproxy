package web

import (
	"embed"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/menno/llmapiproxy/internal/backend"
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
}

func NewUI(cfgMgr *config.Manager, collector *stats.Collector, registry *backend.Registry) *UI {
	return &UI{
		cfgMgr:    cfgMgr,
		collector: collector,
		registry:  registry,
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
		for _, m := range bc.Models {
			prefixed = append(prefixed, bc.Name+"/"+m)
			if bc.IsEnabled() {
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

	data := map[string]any{
		"Backends":    entries,
		"Overlaps":    overlaps,
		"DisplayAddr": displayAddr,
		"SampleModel": sampleModel,
		"Message":     r.URL.Query().Get("msg"),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "models.html", data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// keyEntry holds display data for a single API key on the settings page.
type keyEntry struct {
	Index  int
	Masked string
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
		keys[i] = keyEntry{Index: i, Masked: maskKey(k)}
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
		"Keys":         keys,
		"Backends":     backends,
		"StatsCount":   u.collector.TotalCount(),
		"Message":      msg,
		"IsError":      strings.HasPrefix(msg, "Error"),
		"DisableStats": cfg.Server.DisableStats,
		"ConfigText":   configText,
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
	if len(cfg.Server.APIKeys) <= 1 {
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
