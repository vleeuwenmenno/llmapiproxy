package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/chat"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/identity"
	"github.com/menno/llmapiproxy/internal/stats"
	"github.com/menno/llmapiproxy/internal/users"
	"gopkg.in/yaml.v3"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

var templates *template.Template

// appVersion holds the build version, injected via -ldflags or set at startup.
var appVersion string

// SetVersion sets the application version for display in the web UI.
func SetVersion(v string) {
	appVersion = v
}

func init() {
	templates = template.Must(template.New("").Funcs(template.FuncMap{
		"appVersion": func() string { return appVersion },
		"maskKey":    maskKey,
		// ctxBadge formats a context-length *int64 as a human-readable string (e.g. "128K", "1M").
		"ctxBadge": func(n *int64) string {
			if n == nil {
				return ""
			}
			v := *n
			switch {
			case v >= 1_000_000:
				return fmt.Sprintf("%gM", float64(v)/1_000_000)
			case v >= 1_000:
				return fmt.Sprintf("%gK", float64(v)/1_000)
			default:
				return fmt.Sprintf("%d", v)
			}
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"dict": func(values ...any) (map[string]any, error) {
			if len(values)%2 != 0 {
				return nil, fmt.Errorf("dict requires an even number of arguments")
			}
			out := make(map[string]any, len(values)/2)
			for i := 0; i < len(values); i += 2 {
				key, ok := values[i].(string)
				if !ok {
					return nil, fmt.Errorf("dict keys must be strings")
				}
				out[key] = values[i+1]
			}
			return out, nil
		},
		"hasTime": hasTime,
		"formatTime": func(t time.Time) string {
			return t.Format("15:04:05")
		},
		"timeAgo":     humanizePastTime,
		"timeUntil":   humanizeFutureTime,
		"rfc3339Time": rfc3339Time,
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
		"pctWidth": func(part, total int64) float64 {
			if total <= 0 {
				return 0
			}
			p := float64(part) / float64(total) * 100.0
			if p < 1 && part > 0 {
				p = 1 // minimum visible width
			}
			return p
		},
		"min": func(a, b int) int {
			if a < b {
				return a
			}
			return b
		},
		"splitList": func(sep, s string) []string {
			if s == "" {
				return nil
			}
			return strings.Split(s, sep)
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

func hasTime(t time.Time) bool {
	return !t.IsZero()
}

func rfc3339Time(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func humanizeFutureTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	local := t.Local()
	now := time.Now().In(local.Location())
	if !local.After(now) {
		return humanizePastTime(local)
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	targetDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	dayDiff := int(targetDay.Sub(today) / (24 * time.Hour))

	switch {
	case dayDiff == 0:
		return local.Format("today at 15:04")
	case dayDiff == 1:
		return local.Format("tomorrow at 15:04")
	case dayDiff > 1 && dayDiff < 7:
		return local.Format("Monday at 15:04")
	case local.Year() == now.Year():
		return local.Format("2 Jan at 15:04")
	default:
		return local.Format("2 Jan 2006 at 15:04")
	}
}

func humanizePastTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	local := t.Local()
	now := time.Now().In(local.Location())
	if local.After(now) {
		return humanizeFutureTime(local)
	}

	delta := now.Sub(local)
	switch {
	case delta < time.Minute:
		return "just now"
	case delta < time.Hour:
		minutes := int(delta / time.Minute)
		if minutes <= 1 {
			return "1 minute ago"
		}
		return fmt.Sprintf("%d minutes ago", minutes)
	case delta < 24*time.Hour:
		hours := int(delta / time.Hour)
		if hours <= 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", hours)
	case delta < 48*time.Hour:
		return "yesterday"
	case delta < 7*24*time.Hour:
		days := int(delta / (24 * time.Hour))
		return fmt.Sprintf("%d days ago", days)
	case local.Year() == now.Year():
		return local.Format("2 Jan")
	default:
		return local.Format("2 Jan 2006")
	}
}

type modelOAuthCardData struct {
	Name  string
	Type  string
	OAuth backend.OAuthStatus
}

// CircuitManager is the interface for the circuit breaker system.
type CircuitManager interface {
	AllStates() []CircuitBreakerState
	State(backendName string) CircuitBreakerState
	Reset(backendName string)
	ResetAll()
	UpdateConfig(enabled bool, threshold int, cooldownSec int)
	Enabled() bool
	GetConfig() CircuitBreakerConfig
}

// CircuitBreakerState is a snapshot of a breaker's state for UI display.
type CircuitBreakerState struct {
	Name       string    `json:"name"`
	State      string    `json:"state"`
	Failures   int       `json:"failures"`
	Threshold  int       `json:"threshold"`
	TrippedAt  time.Time `json:"tripped_at,omitempty"`
	RetryAfter time.Time `json:"retry_after,omitempty"`
	Cooldown   string    `json:"cooldown"`
	Reason     string    `json:"reason,omitempty"`
}

// CircuitBreakerConfig holds circuit breaker configuration for the UI.
type CircuitBreakerConfig struct {
	Enabled   bool `json:"enabled"`
	Threshold int  `json:"threshold"`
	Cooldown  int  `json:"cooldown"`
}

type UI struct {
	cfgMgr        *config.Manager
	collector     *stats.Collector
	registry      *backend.Registry
	store         *stats.Store
	chatStore     *chat.ChatStore
	userStore     *users.UserStore
	sessionSecret []byte
	circuit       CircuitManager
}

func NewUI(cfgMgr *config.Manager, collector *stats.Collector, registry *backend.Registry, store *stats.Store, chatStore *chat.ChatStore, userStore *users.UserStore, sessionSecret []byte, circuit CircuitManager) *UI {
	return &UI{
		cfgMgr:        cfgMgr,
		collector:     collector,
		registry:      registry,
		store:         store,
		chatStore:     chatStore,
		userStore:     userStore,
		sessionSecret: sessionSecret,
		circuit:       circuit,
	}
}

// StaticFS returns the embedded static file system.
func StaticFS() embed.FS {
	return staticFS
}

// injectAuth adds the authenticated user to the template data map.
// Returns the same map with a "User" key set if the user is logged in.
func injectAuth(r *http.Request, data map[string]any) map[string]any {
	if data == nil {
		data = make(map[string]any)
	}
	if user := users.UserFromContext(r.Context()); user != nil {
		data["User"] = user
	}
	return data
}

// ── Login / Setup / Logout Handlers ──────────────────────

// LoginPage renders the login form.
func (u *UI) LoginPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Error": "",
		"Next":  r.URL.Query().Get("next"),
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ExecuteTemplate(w, "login.html", data)
}

// LoginPost authenticates a user and sets a session cookie.
func (u *UI) LoginPost(w http.ResponseWriter, r *http.Request) {
	if u.userStore == nil {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	next := r.FormValue("next")
	if next == "" {
		next = "/ui/"
	}

	user, err := u.userStore.Authenticate(username, password)
	if err != nil {
		log.Error().Err(err).Str("username", username).Msg("authentication error")
	}
	if user == nil || err != nil {
		data := map[string]any{
			"Error": "Invalid username or password.",
			"Next":  next,
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		templates.ExecuteTemplate(w, "login.html", data)
		return
	}

	token, err := users.CreateSessionToken(user.Username, u.sessionSecret)
	if err != nil {
		log.Error().Err(err).Msg("failed to create session token")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	secure := strings.HasPrefix(u.cfgMgr.Get().Server.Domain, "https://")
	users.SetSessionCookie(w, token, secure)
	log.Info().Str("username", user.Username).Msg("user logged in")
	http.Redirect(w, r, next, http.StatusFound)
}

// SetupPage renders the first-user setup form. Only shown when web auth is enabled but no users exist.
func (u *UI) SetupPage(w http.ResponseWriter, r *http.Request) {
	if u.userStore == nil {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	count, _ := u.userStore.UserCount()
	if count > 0 {
		// Users already exist, redirect to login
		http.Redirect(w, r, "/ui/login", http.StatusFound)
		return
	}

	data := map[string]any{
		"Error": "",
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ExecuteTemplate(w, "setup.html", data)
}

// SetupPost creates the first admin user and logs them in.
func (u *UI) SetupPost(w http.ResponseWriter, r *http.Request) {
	if u.userStore == nil {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}

	count, _ := u.userStore.UserCount()
	if count > 0 {
		http.Redirect(w, r, "/ui/login", http.StatusFound)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")

	if username == "" || password == "" {
		renderSetupError(w, "Username and password are required.")
		return
	}
	if len(password) < 8 {
		renderSetupError(w, "Password must be at least 8 characters.")
		return
	}
	if password != password2 {
		renderSetupError(w, "Passwords do not match.")
		return
	}

	if err := u.userStore.CreateUser(username, password); err != nil {
		log.Error().Err(err).Str("username", username).Msg("failed to create initial user")
		renderSetupError(w, "Failed to create user: "+err.Error())
		return
	}

	token, err := users.CreateSessionToken(username, u.sessionSecret)
	if err != nil {
		log.Error().Err(err).Msg("failed to create session token")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	secure := strings.HasPrefix(u.cfgMgr.Get().Server.Domain, "https://")
	users.SetSessionCookie(w, token, secure)
	log.Info().Str("username", username).Msg("initial admin user created via setup page")
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

func renderSetupError(w http.ResponseWriter, errMsg string) {
	data := map[string]any{
		"Error": errMsg,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	templates.ExecuteTemplate(w, "setup.html", data)
}

// LogoutPost clears the session cookie and redirects to the login page.
func (u *UI) LogoutPost(w http.ResponseWriter, r *http.Request) {
	users.ClearSessionCookie(w)
	http.Redirect(w, r, "/ui/login", http.StatusFound)
}

const pageSize = 25

func parseWindowParam(s string) (time.Duration, string) {
	switch s {
	case "5m":
		return 5 * time.Minute, "5m"
	case "15m":
		return 15 * time.Minute, "15m"
	case "30m":
		return 30 * time.Minute, "30m"
	case "1h":
		return 1 * time.Hour, "1h"
	case "2h":
		return 2 * time.Hour, "2h"
	case "3h":
		return 3 * time.Hour, "3h"
	case "6h":
		return 6 * time.Hour, "6h"
	case "12h":
		return 12 * time.Hour, "12h"
	case "1d":
		return 24 * time.Hour, "1d"
	case "3d":
		return 3 * 24 * time.Hour, "3d"
	case "7d":
		return 7 * 24 * time.Hour, "7d"
	case "14d":
		return 14 * 24 * time.Hour, "14d"
	case "30d":
		return 30 * 24 * time.Hour, "30d"
	default:
		return 1 * time.Hour, "1h"
	}
}

func statsFilterForWindow(windowDur time.Duration) stats.StatsFilter {
	var f stats.StatsFilter
	if windowDur > 0 {
		f.From = time.Now().Add(-windowDur)
	}
	return f
}

func (u *UI) Dashboard(w http.ResponseWriter, r *http.Request) {
	// Pre-populate filter dropdowns for the template.
	backends, _ := u.store.DistinctValues("backend")
	models, _ := u.store.DistinctValues("model")
	clients, _ := u.store.DistinctValues("client")
	cfg := u.cfgMgr.Get()
	routingJSON, _ := json.Marshal(cfg.Routing)

	data := map[string]any{
		"ActivePage":  "dashboard",
		"Backends":    backends,
		"Models":      models,
		"Clients":     clients,
		"RoutingJSON": template.JS(routingJSON),
	}
	injectAuth(r, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "dashboard.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// unifiedDashResponse is the JSON payload for the consolidated dashboard data endpoint.
type unifiedDashResponse struct {
	Summary     stats.Summary             `json:"summary"`
	Percentiles stats.Percentiles         `json:"percentiles"`
	TimeSeries  []stats.TimePoint         `json:"time_series"`
	TopModels   []stats.RankRow           `json:"top_models"`
	TopBackends []stats.RankRow           `json:"top_backends"`
	TopClients  []stats.RankRow           `json:"top_clients"`
	Routing     []stats.ModelRoutingStats `json:"routing"`
	Records     unifiedRecPage            `json:"records"`
	Filters     unifiedFilters            `json:"filters"`
}

type unifiedRecPage struct {
	Items      []stats.Record `json:"items"`
	Total      int            `json:"total"`
	Page       int            `json:"page"`
	TotalPages int            `json:"total_pages"`
}

type unifiedFilters struct {
	Backends []string `json:"backends"`
	Models   []string `json:"models"`
	Clients  []string `json:"clients"`
}

// DashboardData returns all dashboard data as a single JSON response.
// Supports combinable filters: window, from/to, backend, model, client, errors, page.
func (u *UI) DashboardData(w http.ResponseWriter, r *http.Request) {
	f := parseStatsFilter(r)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 0 {
		page = 0
	}

	summary, err := u.store.FilteredSummary(f)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: summary error")
	}
	pcts, err := u.store.FilteredPercentiles(f)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: percentiles error")
	}
	ts, err := u.store.TimeSeries(f, bucketSecsForFilter(f))
	if err != nil {
		log.Error().Err(err).Msg("dashboard: timeseries error")
	}
	topModels, err := u.store.RankByWithPercentiles(f, "model", 20)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: rank models error")
	}
	topBackends, err := u.store.RankByWithPercentiles(f, "backend", 20)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: rank backends error")
	}
	topClients, err := u.store.RankByWithPercentiles(f, "client", 20)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: rank clients error")
	}
	routing, err := u.store.RoutingStats(f)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: routing stats error")
	}
	if routing == nil {
		routing = []stats.ModelRoutingStats{}
	}
	records, total, err := u.store.FilteredRecords(f, page, pageSize)
	if err != nil {
		log.Error().Err(err).Msg("dashboard: records error")
	}
	totalPages := (total + pageSize - 1) / pageSize

	backends, _ := u.store.DistinctValues("backend")
	models, _ := u.store.DistinctValues("model")
	clients, _ := u.store.DistinctValues("client")

	resp := unifiedDashResponse{
		Summary:     summary,
		Percentiles: pcts,
		TimeSeries:  ts,
		TopModels:   topModels,
		TopBackends: topBackends,
		TopClients:  topClients,
		Routing:     routing,
		Records: unifiedRecPage{
			Items:      records,
			Total:      total,
			Page:       page,
			TotalPages: totalPages,
		},
		Filters: unifiedFilters{
			Backends: backends,
			Models:   models,
			Clients:  clients,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("dashboard data encode error")
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
		log.Error().Err(err).Msg("template error")
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
		log.Error().Err(err).Msg("template error")
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
		"ActivePage": "config",
		"Config":     string(configData),
		"Message":    "",
	}
	injectAuth(r, data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "config.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
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
	Name            string
	Type            string // "openai", "anthropic", "copilot", "codex", "ollama"
	BaseURL         string
	APIKey          string       // masked API key for pre-filling the switch-type modal
	Models          []ModelEntry // enriched model metadata (nil for dynamic backends)
	IsDynamic       bool         // true when no explicit model list (accepts all)
	IconURL         string       // path to SVG icon, empty if unknown
	Enabled         bool         // false when backend is explicitly disabled
	StaticCount     int          // pre-computed count for statically-configured backends
	DisabledModels  []string     // model IDs disabled on this backend
	IdentityProfile string       // per-backend identity profile override (empty = use global)
	CircuitOpen     bool         // true when circuit breaker is tripped
	CompatMode      string       // ollama compat mode: "openai", "anthropic", "native"
}

// ModelEntry holds display data for a single model in the UI.
type ModelEntry struct {
	FullID          string   `json:"full_id"` // backend/model-id
	BareID          string   `json:"bare_id"` // model-id without backend prefix
	ContextLength   *int64   `json:"context_length,omitempty"`
	MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	DataSource      string   `json:"data_source,omitempty"` // "upstream", "config", "builtin", or ""
	Disabled        bool     `json:"disabled,omitempty"`    // true when this model is in the backend's disabled_models list
	Alias           string   `json:"alias,omitempty"`       // non-empty when this model is aliased to another name
}

// iconForBackend maps a backend name to a static icon URL.
func iconForBackend(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "openrouter"):
		return "/ui/static/icons/openrouter.svg"
	case strings.Contains(n, "zai") || strings.Contains(n, "z.ai"):
		return "/ui/static/icons/zai.svg"
	case strings.Contains(n, "openai"):
		return "/ui/static/icons/openai.svg"
	case strings.Contains(n, "claude") || strings.Contains(n, "anthropic"):
		return "/ui/static/icons/claude.svg"
	case strings.Contains(n, "ollama"):
		return "/ui/static/icons/ollama.svg"
	case strings.Contains(n, "zen") || strings.Contains(n, "opencode"):
		return "/ui/static/icons/openai.svg"
	case strings.Contains(n, "codex"):
		return "/ui/static/icons/codex-color.svg"
	case strings.Contains(n, "copilot"):
		return "/ui/static/icons/githubcopilot.svg"
	case strings.Contains(n, "gemini"):
		return "/ui/static/icons/geminicli-color.svg"
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

// circuitStatesByBackend returns a map of backend name → circuit breaker open state.
// Only includes backends where the circuit is tripped (open or half-open).
func (u *UI) circuitStatesByBackend(cfg *config.Config) map[string]bool {
	if u.circuit == nil || !u.circuit.Enabled() {
		return nil
	}
	states := make(map[string]bool)
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		s := u.circuit.State(bc.Name)
		if s.State == "open" {
			states[bc.Name] = true
		}
	}
	if len(states) == 0 {
		return nil
	}
	return states
}

func (u *UI) ModelsPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Build skeleton entries from config only — no network calls.
	// Model metadata is loaded lazily per-card by the browser via BackendModels.
	entries := make([]BackendEntry, 0, len(cfg.Backends))

	for _, bc := range cfg.Backends {
		isDynamic := len(bc.Models) == 0

		// For static backends, build config-only entries (no live metadata yet).
		// These render immediately; JS will update them with metadata badges.
		var modelEntries []ModelEntry
		if !isDynamic {
			seenModelIDs := make(map[string]bool, len(bc.Models))
			disabledSet := make(map[string]bool, len(bc.DisabledModels))
			for _, dm := range bc.DisabledModels {
				disabledSet[dm] = true
			}
			for _, mc := range bc.Models {
				if seenModelIDs[mc.ID] {
					continue
				}
				seenModelIDs[mc.ID] = true
				alias := ""
				if bc.ModelAliases != nil {
					alias = bc.ModelAliases[mc.ID]
				}
				modelEntries = append(modelEntries, ModelEntry{
					FullID:   bc.Name + "/" + mc.ID,
					BareID:   mc.ID,
					Disabled: disabledSet[mc.ID],
					Alias:    alias,
				})
			}
		}

		entries = append(entries, BackendEntry{
			Name:            bc.Name,
			Type:            bc.Type,
			BaseURL:         bc.BaseURL,
			APIKey:          maskKey(bc.APIKey),
			Models:          modelEntries, // nil for dynamic; IDs-only for static
			IsDynamic:       isDynamic,
			IconURL:         iconForBackend(bc.Name),
			Enabled:         bc.IsEnabled(),
			StaticCount:     len(modelEntries),
			DisabledModels:  bc.DisabledModels,
			IdentityProfile: bc.IdentityProfile,
			CircuitOpen:     u.circuit != nil && u.circuit.Enabled() && u.circuit.State(bc.Name).State == "open",
			CompatMode:      bc.CompatMode,
		})
	}

	// Compute overlaps from the ModelIndex (live data, includes dynamic backends
	// and applies canonicalization rules like Ollama tag stripping).
	// Falls back to config-only detection if the index isn't ready yet.
	var overlaps []OverlapEntry
	if idx := u.registry.ModelIndex(); idx != nil {
		indexOverlaps := idx.Overlaps()
		log.Debug().Int("count", len(indexOverlaps)).Dur("index_age", idx.Age()).Msg("models page: using ModelIndex overlaps")
		for _, im := range indexOverlaps {
			backendModels := make(map[string]string, len(im.Backends))
			backendNames := make([]string, 0, len(im.Backends))
			for _, ref := range im.Backends {
				backendNames = append(backendNames, ref.BackendName)
				backendModels[ref.BackendName] = ref.RawModelID
			}
			log.Debug().Str("model", im.CanonicalID).Strs("backends", backendNames).Msg("models page: overlap entry")
			overlaps = append(overlaps, OverlapEntry{
				ModelID:       im.CanonicalID,
				Backends:      backendNames,
				BackendModels: backendModels,
			})
		}
	} else {
		log.Debug().Msg("models page: ModelIndex nil, using fallback overlap detection")
		// Fallback: config-only overlap detection (before model caches are warmed).
		modelBackends := make(map[string][]string)
		backendModelIDs := make(map[string]map[string]string)
		for _, bc := range cfg.Backends {
			if !bc.IsEnabled() {
				continue
			}
			for _, mc := range bc.Models {
				canonical := lastPathSegment(mc.ID)
				modelBackends[canonical] = append(modelBackends[canonical], bc.Name)
				if backendModelIDs[canonical] == nil {
					backendModelIDs[canonical] = make(map[string]string)
				}
				backendModelIDs[canonical][bc.Name] = mc.ID
			}
		}
		for canonicalID, backends := range modelBackends {
			if len(backends) >= 2 {
				overlaps = append(overlaps, OverlapEntry{
					ModelID:       canonicalID,
					Backends:      backends,
					BackendModels: backendModelIDs[canonicalID],
				})
			}
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
		Backends         []string `json:"backends"`
		Strategy         string   `json:"strategy"`
		DisabledBackends []string `json:"disabled_backends,omitempty"`
	}
	routingByModel := make(map[string]routingModelData)
	for _, mr := range cfg.Routing.Models {
		routingByModel[mr.Model] = routingModelData{Backends: mr.Backends, Strategy: mr.Strategy, DisabledBackends: mr.DisabledBackends}
	}

	routingJSON, _ := json.Marshal(routingByModel)

	// Build disabled models map per backend for the JS UI.
	disabledModelsByBackend := make(map[string]map[string]bool)
	for _, bc := range cfg.Backends {
		if len(bc.DisabledModels) > 0 {
			m := make(map[string]bool, len(bc.DisabledModels))
			for _, dm := range bc.DisabledModels {
				m[dm] = true
			}
			disabledModelsByBackend[bc.Name] = m
		}
	}
	disabledModelsJSON, _ := json.Marshal(disabledModelsByBackend)

	modelAliasesByBackend := make(map[string]map[string]string)
	for _, bc := range cfg.Backends {
		if len(bc.ModelAliases) > 0 {
			modelAliasesByBackend[bc.Name] = bc.ModelAliases
		}
	}
	modelAliasesJSON, _ := json.Marshal(modelAliasesByBackend)

	var aliasCollisions []backend.AliasCollision
	if u.registry != nil {
		aliasCollisions = u.registry.ModelIndex().Collisions()
	}
	aliasCollisionsJSON, _ := json.Marshal(aliasCollisions)

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
			if bc.IsModelDisabled(m.ID) {
				continue
			}
			curlModels = append(curlModels, bc.Name+"/"+m.ID)
		}
	}

	// Build a map of backend name → *OAuthStatus for quick lookup in templates.
	// Using a pointer so that missing keys return nil (falsy in templates),
	// preventing zero-value structs from matching {{if $oauth}} for non-OAuth backends.
	oauthStatuses := u.registry.OAuthStatuses()
	oauthByBackend := make(map[string]*backend.OAuthStatus, len(oauthStatuses))
	for i := range oauthStatuses {
		oauthByBackend[oauthStatuses[i].BackendName] = &oauthStatuses[i]
	}

	// Build identity profile list for the models page selector.
	var customProfiles []identity.Profile
	for _, cp := range cfg.CustomIdentityProfiles {
		customProfiles = append(customProfiles, identity.Profile{
			ID:          cp.ID,
			DisplayName: cp.DisplayName,
			UserAgent:   cp.UserAgent,
			Headers:     cp.Headers,
		})
	}
	allIdentityProfiles := identity.AllProfiles(customProfiles)

	// Build the exposed models table from FlatModelList.
	var exposedModels []ExposedModelEntry
	if u.registry != nil {
		flatModels := u.registry.FlatModelList(r.Context(), cfg.Routing)
		exposedModels = make([]ExposedModelEntry, 0, len(flatModels))
		for _, m := range flatModels {
			caps := m.Capabilities
			if caps == nil {
				caps = []string{}
			}
			// Build rawIDs map: for each serving backend, look up the raw model ID from index.
			rawIDs := make(map[string]string)
			if idx := u.registry.ModelIndex(); idx != nil {
				for _, im := range idx.FlatModels() {
					if im.CanonicalID == m.ID {
						for _, ref := range im.Backends {
							rawIDs[ref.BackendName] = ref.RawModelID
						}
						break
					}
				}
			}
			// Fall back to canonical ID for backends without index data.
			for _, bn := range m.AvailableBackends {
				if _, ok := rawIDs[bn]; !ok {
					rawIDs[bn] = m.ID
				}
			}
			displayStrat := m.RoutingStrategy
			if displayStrat == "" {
				displayStrat = cfg.Routing.Strategy
			}
			if displayStrat == "" {
				displayStrat = "priority"
			}
		// Collect any aliases applied to this model across all backends.
		var aliases []string
		var originalIDs []string
		seenAlias := make(map[string]bool)
		seenOrig := make(map[string]bool)
		for _, bn := range m.AvailableBackends {
			rawID := rawIDs[bn]
			if aliasMap, ok := modelAliasesByBackend[bn]; ok {
				if a, ok := aliasMap[rawID]; ok && a != "" && !seenAlias[a] {
					aliases = append(aliases, a)
					seenAlias[a] = true
					// Track original raw ID so the template can show it as a badge.
					if rawID != a && !seenOrig[rawID] {
						originalIDs = append(originalIDs, rawID)
						seenOrig[rawID] = true
					}
				}
			}
		}
			exposedModels = append(exposedModels, ExposedModelEntry{
				CanonicalID:     m.ID,
				DisplayName:     m.DisplayName,
				Backends:        m.AvailableBackends,
				RawIDs:          rawIDs,
				Aliases:         aliases,
				OriginalIDs:     originalIDs,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
				Capabilities:    caps,
				RoutingStrategy: displayStrat,
			})
		}
	}

	data := map[string]any{
		"ActivePage":            "models",
		"Backends":              entries,
		"Overlaps":              overlaps,
		"ExposedModels":         exposedModels,
		"DisplayAddr":           displayAddr,
		"SampleModel":           sampleModel,
		"Message":               r.URL.Query().Get("msg"),
		"RoutingJSON":           template.JS(routingJSON),
		"GlobalStrategy":        cfg.Routing.Strategy,
		"DisabledModelsJSON":    template.JS(disabledModelsJSON),
		"ModelAliasesJSON":      template.JS(modelAliasesJSON),
		"AliasCollisionsJSON":   template.JS(aliasCollisionsJSON),
		"AliasCollisions":       aliasCollisions,
		"ServerAPIKeys":         apiKeyEntries,
		"CurlModels":            curlModels,
		"OAuthByBackend":        oauthByBackend,
		"IdentityProfiles":      allIdentityProfiles,
		"GlobalIdentityProfile": cfg.IdentityProfile,
		"CircuitByBackend":      u.circuitStatesByBackend(cfg),
	}
	injectAuth(r, data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "models.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// ExposedModelEntry is a row in the "Exposed Models" table on the models page.
// It represents one canonical model ID as returned by /v1/models.
type ExposedModelEntry struct {
	CanonicalID     string            // model ID as exposed via /v1/models
	DisplayName     string
	Backends        []string          // backend names that serve this model, in routing priority order
	RawIDs          map[string]string // backend → raw model ID
	Aliases         []string          // alias names applied to this model (may come from multiple backends)
	OriginalIDs     []string          // original raw model IDs before aliasing (deduplicated)
	ContextLength   *int64
	MaxOutputTokens *int64
	Capabilities    []string
	RoutingStrategy string // effective strategy (per-model override or global)
}

// BackendsPage renders the backends management page (backend cards + quick connect).
func (u *UI) BackendsPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	entries := make([]BackendEntry, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		isDynamic := len(bc.Models) == 0
		var modelEntries []ModelEntry
		if !isDynamic {
			seenModelIDs := make(map[string]bool, len(bc.Models))
			disabledSet := make(map[string]bool, len(bc.DisabledModels))
			for _, dm := range bc.DisabledModels {
				disabledSet[dm] = true
			}
			for _, mc := range bc.Models {
				if seenModelIDs[mc.ID] {
					continue
				}
				seenModelIDs[mc.ID] = true
				alias := ""
				if bc.ModelAliases != nil {
					alias = bc.ModelAliases[mc.ID]
				}
				modelEntries = append(modelEntries, ModelEntry{
					FullID:   bc.Name + "/" + mc.ID,
					BareID:   mc.ID,
					Disabled: disabledSet[mc.ID],
					Alias:    alias,
				})
			}
		}
		entries = append(entries, BackendEntry{
			Name:            bc.Name,
			Type:            bc.Type,
			BaseURL:         bc.BaseURL,
			APIKey:          maskKey(bc.APIKey),
			Models:          modelEntries,
			IsDynamic:       isDynamic,
			IconURL:         iconForBackend(bc.Name),
			Enabled:         bc.IsEnabled(),
			StaticCount:     len(modelEntries),
			DisabledModels:  bc.DisabledModels,
			IdentityProfile: bc.IdentityProfile,
			CircuitOpen:     u.circuit != nil && u.circuit.Enabled() && u.circuit.State(bc.Name).State == "open",
			CompatMode:      bc.CompatMode,
		})
	}

	listen := cfg.Server.Listen
	displayAddr := "localhost" + listen
	if strings.HasPrefix(listen, "0.0.0.0") {
		displayAddr = "localhost" + listen[len("0.0.0.0"):]
	} else if !strings.Contains(listen, ":") {
		displayAddr = listen
	}

	sampleModel := "backend/model-id"
	for _, e := range entries {
		if e.Enabled && len(e.Models) > 0 {
			sampleModel = e.Models[0].FullID
			break
		}
	}

	type routingModelData struct {
		Backends         []string `json:"backends"`
		Strategy         string   `json:"strategy"`
		DisabledBackends []string `json:"disabled_backends,omitempty"`
	}
	routingByModel := make(map[string]routingModelData)
	for _, mr := range cfg.Routing.Models {
		routingByModel[mr.Model] = routingModelData{Backends: mr.Backends, Strategy: mr.Strategy, DisabledBackends: mr.DisabledBackends}
	}
	routingJSON, _ := json.Marshal(routingByModel)

	disabledModelsByBackend := make(map[string]map[string]bool)
	for _, bc := range cfg.Backends {
		if len(bc.DisabledModels) > 0 {
			m := make(map[string]bool, len(bc.DisabledModels))
			for _, dm := range bc.DisabledModels {
				m[dm] = true
			}
			disabledModelsByBackend[bc.Name] = m
		}
	}
	disabledModelsJSON, _ := json.Marshal(disabledModelsByBackend)

	modelAliasesByBackend := make(map[string]map[string]string)
	for _, bc := range cfg.Backends {
		if len(bc.ModelAliases) > 0 {
			modelAliasesByBackend[bc.Name] = bc.ModelAliases
		}
	}
	modelAliasesJSON, _ := json.Marshal(modelAliasesByBackend)

	// Pre-seed allBackendModels for non-dynamic backends so aliases show
	// immediately on page load without waiting for the async API fetch.
	staticBackendModels := make(map[string][]ModelEntry)
	for _, e := range entries {
		if !e.IsDynamic && len(e.Models) > 0 {
			staticBackendModels[e.Name] = e.Models
		}
	}
	staticBackendModelsJSON, _ := json.Marshal(staticBackendModels)

	var aliasCollisions []backend.AliasCollision
	if u.registry != nil {
		aliasCollisions = u.registry.ModelIndex().Collisions()
	}
	aliasCollisionsJSON, _ := json.Marshal(aliasCollisions)

	apiKeyEntries := make([]keyEntry, len(cfg.Server.APIKeys))
	for i, k := range cfg.Server.APIKeys {
		apiKeyEntries[i] = keyEntry{Index: i, Masked: maskKey(k), Full: k}
	}

	var curlModels []string
	for _, bc := range cfg.Backends {
		if !bc.IsEnabled() {
			continue
		}
		for _, m := range bc.Models {
			if bc.IsModelDisabled(m.ID) {
				continue
			}
			curlModels = append(curlModels, bc.Name+"/"+m.ID)
		}
	}

	oauthStatuses := u.registry.OAuthStatuses()
	oauthByBackend := make(map[string]*backend.OAuthStatus, len(oauthStatuses))
	for i := range oauthStatuses {
		oauthByBackend[oauthStatuses[i].BackendName] = &oauthStatuses[i]
	}

	var customProfiles []identity.Profile
	for _, cp := range cfg.CustomIdentityProfiles {
		customProfiles = append(customProfiles, identity.Profile{
			ID:          cp.ID,
			DisplayName: cp.DisplayName,
			UserAgent:   cp.UserAgent,
			Headers:     cp.Headers,
		})
	}
	allIdentityProfiles := identity.AllProfiles(customProfiles)

	data := map[string]any{
		"ActivePage":             "backends",
		"Backends":                entries,
		"DisplayAddr":             displayAddr,
		"SampleModel":             sampleModel,
		"Message":                 r.URL.Query().Get("msg"),
		"RoutingJSON":             template.JS(routingJSON),
		"GlobalStrategy":          cfg.Routing.Strategy,
		"DisabledModelsJSON":      template.JS(disabledModelsJSON),
		"ModelAliasesJSON":        template.JS(modelAliasesJSON),
		"StaticBackendModelsJSON": template.JS(staticBackendModelsJSON),
		"AliasCollisionsJSON":     template.JS(aliasCollisionsJSON),
		"AliasCollisions":         aliasCollisions,
		"ServerAPIKeys":           apiKeyEntries,
		"CurlModels":              curlModels,
		"OAuthByBackend":          oauthByBackend,
		"IdentityProfiles":        allIdentityProfiles,
		"GlobalIdentityProfile":   cfg.IdentityProfile,
		"CircuitByBackend":        u.circuitStatesByBackend(cfg),
	}
	injectAuth(r, data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "backends.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
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

	disabledSet := make(map[string]bool, len(bc.DisabledModels))
	for _, dm := range bc.DisabledModels {
		disabledSet[dm] = true
	}

	if len(disabledSet) > 0 {
		log.Debug().Str("backend", name).Int("disabled_count", len(disabledSet)).Interface("disabled_models", bc.DisabledModels).Msg("BackendModels disabled set")
	}

	if isDynamic {
		for _, m := range liveModels {
			alias := ""
			if bc.ModelAliases != nil {
				alias = bc.ModelAliases[m.ID]
			}
			entries = append(entries, ModelEntry{
				FullID:          bc.Name + "/" + m.ID,
				BareID:          m.ID,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
				Capabilities:    m.Capabilities,
				DataSource:      "upstream",
				Disabled:        m.Disabled || disabledSet[m.ID],
				Alias:           alias,
			})
		}
	} else {
		for _, mc := range bc.Models {
			entry := ModelEntry{
				FullID:   bc.Name + "/" + mc.ID,
				BareID:   mc.ID,
				Disabled: disabledSet[mc.ID],
			}
			if bc.ModelAliases != nil {
				entry.Alias = bc.ModelAliases[mc.ID]
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
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		log.Error().Err(err).Msg("BackendModels encode error")
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

	// Rebuild the model index asynchronously so overlap detection and
	// routing reflect the refreshed model list immediately.
	go u.registry.RebuildIndex()

	// Look up alias map for this backend from config.
	cfg := u.cfgMgr.Get()
	var aliasMap map[string]string
	for _, bc := range cfg.Backends {
		if bc.Name == name {
			aliasMap = bc.ModelAliases
			break
		}
	}

	type modelResp struct {
		ID              string   `json:"id"`
		DisplayName     string   `json:"display_name,omitempty"`
		ContextLength   *int64   `json:"context_length,omitempty"`
		MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
		Capabilities    []string `json:"capabilities,omitempty"`
		Disabled        bool     `json:"disabled,omitempty"`
		Alias           string   `json:"alias,omitempty"`
	}

	resp := make([]modelResp, len(models))
	for i, m := range models {
		alias := ""
		if aliasMap != nil {
			alias = aliasMap[m.ID]
		}
		resp[i] = modelResp{
			ID:              name + "/" + m.ID,
			DisplayName:     m.DisplayName,
			ContextLength:   m.ContextLength,
			MaxOutputTokens: m.MaxOutputTokens,
			Capabilities:    m.Capabilities,
			Disabled:        m.Disabled,
			Alias:           alias,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// BackendUpstreamModels returns the raw upstream /models API response for a single
// named backend. This is useful for debugging why a dynamic backend shows no models.
func (u *UI) BackendUpstreamModels(w http.ResponseWriter, r *http.Request) {
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

	provider, ok := b.(backend.UpstreamModelsProvider)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"backend": name,
			"status":  "unsupported",
			"error":   "this backend type does not support raw upstream model inspection",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	result, err := provider.FetchUpstreamModelsRaw(ctx)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"backend": name,
			"status":  "error",
			"error":   err.Error(),
		})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// playgroundModel is a compact model descriptor for the chat JS.
type playgroundModel struct {
	ID                string   `json:"id"`
	ContextLength     *int64   `json:"context_length,omitempty"`
	MaxOutputTokens   *int64   `json:"max_output_tokens,omitempty"`
	DisplayName       string   `json:"display_name,omitempty"`
	AvailableBackends []string `json:"available_backends,omitempty"`
	RoutingStrategy   string   `json:"routing_strategy,omitempty"`
}

// ── Chat API Handlers ──────────────────────────────────────────────────────

// ChatPage renders the interactive chat UI (sessions + messages).
func (u *UI) ChatPage(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()

	// Find the API key for the playground. Prefer a named "playground"
	// client, but fall back to the first server.api_keys entry if no
	// playground client is configured.
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
		"ActivePage":   "chat",
		"ChatAPIKey":   playgroundAPIKey,
		"Models":       models,
		"TitleModel":   cfg.Server.TitleModel,
		"DefaultModel": cfg.Server.DefaultModel,
	}

	injectAuth(r, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "chat.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// ChatModels returns a JSON list of all models from enabled backends with metadata.
// Supports ?mode=raw (default, backend-prefixed IDs) and ?mode=flat (deduplicated with routing).
func (u *UI) ChatModels(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "flat" {
		u.chatModelsFlat(w, r)
		return
	}
	u.chatModelsRaw(w, r)
}

func (u *UI) chatModelsRaw(w http.ResponseWriter, r *http.Request) {
	var models []playgroundModel
	for _, b := range u.registry.All() {
		list, err := b.ListModels(r.Context())
		if err != nil {
			log.Warn().Err(err).Str("backend", b.Name()).Msg("chat: error listing models")
			continue
		}
		for _, m := range list {
			id := m.ID
			if !strings.HasPrefix(id, b.Name()+"/") {
				id = b.Name() + "/" + id
			}
			models = append(models, playgroundModel{
				ID:              id,
				ContextLength:   m.ContextLength,
				MaxOutputTokens: m.MaxOutputTokens,
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

func (u *UI) chatModelsFlat(w http.ResponseWriter, r *http.Request) {
	routing := u.cfgMgr.Get().Routing
	allModels := u.registry.FlatModelList(r.Context(), routing)

	models := make([]playgroundModel, 0, len(allModels))
	for _, m := range allModels {
		models = append(models, playgroundModel{
			ID:                m.ID,
			ContextLength:     m.ContextLength,
			MaxOutputTokens:   m.MaxOutputTokens,
			DisplayName:       m.DisplayName,
			AvailableBackends: m.AvailableBackends,
			RoutingStrategy:   m.RoutingStrategy,
		})
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
	log.Debug().Str("session", id).Str("config_title_model", titleModel).Str("session_model", session.Model).Msg("ChatGenerateTitle: resolving model")
	if titleModel == "" {
		// Fallback: use the session's model, or just truncate.
		titleModel = session.Model
		log.Debug().Str("model", titleModel).Msg("ChatGenerateTitle: using session model (config title_model empty)")
	}
	if titleModel == "" {
		// Last resort: just use truncated first user message.
		log.Warn().Msg("ChatGenerateTitle: no model available, using fallback title")
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
	log.Debug().Str("backend", backendName).Str("model", modelID).Msg("ChatGenerateTitle: resolved backend")

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
		log.Warn().Msg("ChatGenerateTitle: backend/model not found, using fallback title")
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
	log.Debug().Str("url", targetBackendCfg.BaseURL).Str("model", modelID).Msg("ChatGenerateTitle: calling LLM")

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
		log.Error().Err(err).Msg("ChatGenerateTitle: LLM call failed")
		title := generateFallbackTitle(messages)
		_ = u.chatStore.UpdateSessionTitle(id, title)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"title": title})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Error().Int("status", resp.StatusCode).Str("body", string(body)).Msg("ChatGenerateTitle: LLM returned error status")
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
		log.Error().Err(err).Int("choices", len(titleResp.Choices)).Msg("ChatGenerateTitle: failed to decode LLM response")
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
	log.Debug().Str("title", title).Msg("ChatGenerateTitle: generated title")

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
		"ActivePage":     "settings",
		"LegacyKeys":     keys, // server.api_keys entries (unnamed, for migration notice only)
		"Backends":       backends,
		"StatsCount":     u.collector.TotalCount(),
		"Message":        msg,
		"IsError":        strings.HasPrefix(msg, "Error"),
		"DisableStats":   cfg.Server.DisableStats,
		"ConfigText":     configText,
		"Clients":        visibleClients,
		"ClientsJSON":    template.JS(func() []byte { b, _ := json.Marshal(visibleClients); return b }()),
		"ServerHost":     cfg.Server.Host,
		"ServerPort":     cfg.Server.Port,
		"ModelCacheTTL":  cfg.Server.ModelCacheTTL.String(),
		"RoutingJSON":    template.JS(func() []byte { b, _ := json.Marshal(cfg.Routing); return b }()),
		"CircuitEnabled": u.circuit != nil && u.circuit.Enabled(),
		"CircuitThreshold": func() int {
			if u.circuit != nil {
				return u.circuit.GetConfig().Threshold
			}
			return 3
		}(),
		"CircuitCooldown": func() int {
			if u.circuit != nil {
				return u.circuit.GetConfig().Cooldown
			}
			return 300
		}(),
	}
	injectAuth(r, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "settings.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
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

func (u *UI) ExportOverview(w http.ResponseWriter, r *http.Request) {
	f := parseStatsFilter(r)
	format := r.URL.Query().Get("format")
	if format != "md" {
		format = "csv"
	}

	summary, err := u.store.FilteredSummary(f)
	if err != nil {
		log.Error().Err(err).Msg("export overview: summary error")
	}
	pcts, err := u.store.FilteredPercentiles(f)
	if err != nil {
		log.Error().Err(err).Msg("export overview: percentiles error")
	}
	topModels, _ := u.store.RankByWithPercentiles(f, "model", 20)
	topBackends, _ := u.store.RankByWithPercentiles(f, "backend", 20)
	topClients, _ := u.store.RankByWithPercentiles(f, "client", 20)

	filterDesc := describeFilter(f)
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	var body string
	if format == "md" {
		body = buildOverviewMarkdown(summary, pcts, topModels, topBackends, topClients, filterDesc, now)
	} else {
		body = buildOverviewCSV(summary, pcts, topModels, topBackends, topClients, filterDesc, now)
	}

	ext := "." + format
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", "attachment; filename=dashboard-overview-"+time.Now().Format("2006-01-02")+ext)
	w.Write([]byte(body))
}

func (u *UI) ExportLogSummary(w http.ResponseWriter, r *http.Request) {
	f := parseStatsFilter(r)
	format := r.URL.Query().Get("format")
	if format != "md" {
		format = "csv"
	}
	groupBy := r.URL.Query().Get("group_by")
	if groupBy == "" {
		groupBy = "model"
	}

	rows, err := u.store.AggregateBy(f, groupBy)
	if err != nil {
		log.Error().Err(err).Str("group_by", groupBy).Msg("export log summary: aggregate error")
		http.Error(w, "aggregation error", http.StatusInternalServerError)
		return
	}

	filterDesc := describeFilter(f)
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	var body string
	if format == "md" {
		body = buildLogSummaryMarkdown(rows, groupBy, filterDesc, now)
	} else {
		body = buildLogSummaryCSV(rows, groupBy, filterDesc, now)
	}

	ext := "." + format
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", "attachment; filename=log-summary-"+groupBy+"-"+time.Now().Format("2006-01-02")+ext)
	w.Write([]byte(body))
}

func describeFilter(f stats.StatsFilter) string {
	var parts []string
	if !f.From.IsZero() {
		parts = append(parts, "From: "+f.From.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if !f.To.IsZero() {
		parts = append(parts, "To: "+f.To.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if f.Backend != "" {
		parts = append(parts, "Backend: "+f.Backend)
	}
	if f.Model != "" {
		parts = append(parts, "Model: "+f.Model)
	}
	if f.Client != "" {
		parts = append(parts, "Client: "+f.Client)
	}
	if f.ErrOnly {
		parts = append(parts, "Errors only")
	}
	if len(parts) == 0 {
		return "All time"
	}
	return strings.Join(parts, ", ")
}

func buildOverviewCSV(s stats.Summary, p stats.Percentiles, models, backends, clients []stats.RankRow, filterDesc, ts string) string {
	var b strings.Builder
	b.WriteString("LLM API Proxy - Dashboard Export\r\n")
	b.WriteString("Generated," + ts + "\r\n")
	b.WriteString("Filters," + csvEsc(filterDesc) + "\r\n")
	b.WriteString("\r\n")

	b.WriteString("## Summary\r\n")
	b.WriteString("Metric,Value\r\n")
	b.WriteString(fmt.Sprintf("Requests,%d\r\n", s.TotalRequests))
	b.WriteString(fmt.Sprintf("Total Tokens,%d\r\n", s.TotalTokens))
	b.WriteString(fmt.Sprintf("Avg Latency,%dms\r\n", s.AvgLatencyMs))
	b.WriteString(fmt.Sprintf("P50 Latency,%dms\r\n", p.P50))
	b.WriteString(fmt.Sprintf("P90 Latency,%dms\r\n", p.P90))
	b.WriteString(fmt.Sprintf("P99 Latency,%dms\r\n", p.P99))
	b.WriteString(fmt.Sprintf("Errors,%d\r\n", s.TotalErrors))
	if s.TotalRequests > 0 {
		b.WriteString(fmt.Sprintf("Error %%,%.1f%%\r\n", float64(s.TotalErrors)/float64(s.TotalRequests)*100))
	}
	b.WriteString(fmt.Sprintf("Cached Tokens,%d\r\n", s.TotalCached))
	b.WriteString(fmt.Sprintf("Reasoning Tokens,%d\r\n", s.TotalReasoning))
	if s.AvgTPS > 0 {
		b.WriteString(fmt.Sprintf("Avg TPS,%.1f tok/s\r\n", s.AvgTPS))
	}
	if s.AvgTTFTMs > 0 {
		b.WriteString(fmt.Sprintf("Avg TTFT,%dms\r\n", s.AvgTTFTMs))
	}
	if s.AvgGenerationMs > 0 {
		b.WriteString(fmt.Sprintf("Avg Generation,%dms\r\n", s.AvgGenerationMs))
	}
	b.WriteString("\r\n")

	b.WriteString("## Models Ranking\r\n")
	writeRankCSV(&b, models)
	b.WriteString("\r\n")

	b.WriteString("## Backends Ranking\r\n")
	writeRankCSV(&b, backends)
	b.WriteString("\r\n")

	b.WriteString("## Clients Ranking\r\n")
	writeRankCSV(&b, clients)

	return b.String()
}

func writeRankCSV(b *strings.Builder, rows []stats.RankRow) {
	b.WriteString("Name,Requests,Tokens,Errors,Error %,Avg Latency (ms),P50 (ms),P90 (ms),P99 (ms),Avg TTFT (ms),Avg Gen (ms)\r\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%s,%d,%d,%d,%.1f,%d,%d,%d,%d,%d,%d\r\n",
			csvEsc(r.Name), r.Requests, r.Tokens, r.Errors, r.ErrPct,
			r.AvgLatMs, r.P50, r.P90, r.P99, r.AvgTTFTMs, r.AvgGenerationMs))
	}
}

func buildOverviewMarkdown(s stats.Summary, p stats.Percentiles, models, backends, clients []stats.RankRow, filterDesc, ts string) string {
	var b strings.Builder
	b.WriteString("# LLM API Proxy — Dashboard Export\n\n")
	b.WriteString(fmt.Sprintf("**Generated**: %s  \n", ts))
	b.WriteString(fmt.Sprintf("**Filters**: %s\n\n", filterDesc))

	b.WriteString("## Summary\n\n")
	b.WriteString("| Metric | Value |\n|--------|-------|\n")
	b.WriteString(fmt.Sprintf("| Requests | %s |\n", humanInt(s.TotalRequests)))
	b.WriteString(fmt.Sprintf("| Total Tokens | %s |\n", humanInt(s.TotalTokens)))
	b.WriteString(fmt.Sprintf("| Avg Latency | %dms |\n", s.AvgLatencyMs))
	b.WriteString(fmt.Sprintf("| P50 Latency | %dms |\n", p.P50))
	b.WriteString(fmt.Sprintf("| P90 Latency | %dms |\n", p.P90))
	b.WriteString(fmt.Sprintf("| P99 Latency | %dms |\n", p.P99))
	b.WriteString(fmt.Sprintf("| Errors | %s |\n", humanInt(s.TotalErrors)))
	if s.TotalRequests > 0 {
		b.WriteString(fmt.Sprintf("| Error %% | %.1f%% |\n", float64(s.TotalErrors)/float64(s.TotalRequests)*100))
	}
	b.WriteString(fmt.Sprintf("| Cached Tokens | %s |\n", humanInt(s.TotalCached)))
	b.WriteString(fmt.Sprintf("| Reasoning Tokens | %s |\n", humanInt(s.TotalReasoning)))
	if s.AvgTPS > 0 {
		b.WriteString(fmt.Sprintf("| Avg TPS | %.1f tok/s |\n", s.AvgTPS))
	}
	if s.AvgTTFTMs > 0 {
		b.WriteString(fmt.Sprintf("| Avg TTFT | %dms |\n", s.AvgTTFTMs))
	}
	if s.AvgGenerationMs > 0 {
		b.WriteString(fmt.Sprintf("| Avg Generation | %dms |\n", s.AvgGenerationMs))
	}
	b.WriteString("\n")

	b.WriteString("## Models Ranking\n\n")
	writeRankMarkdown(&b, models)
	b.WriteString("\n")

	b.WriteString("## Backends Ranking\n\n")
	writeRankMarkdown(&b, backends)
	b.WriteString("\n")

	b.WriteString("## Clients Ranking\n\n")
	writeRankMarkdown(&b, clients)

	return b.String()
}

func writeRankMarkdown(b *strings.Builder, rows []stats.RankRow) {
	b.WriteString("| Name | Requests | Tokens | Errors | Error % | Avg Latency | P50 | P90 | P99 | TTFT | Gen |\n")
	b.WriteString("|------|----------|--------|--------|---------|-------------|-----|-----|-----|------|-----|\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %.1f%% | %dms | %dms | %dms | %dms | %dms | %dms |\n",
			r.Name, humanInt(r.Requests), humanInt(r.Tokens), r.Errors, r.ErrPct,
			r.AvgLatMs, r.P50, r.P90, r.P99, r.AvgTTFTMs, r.AvgGenerationMs))
	}
}

func buildLogSummaryCSV(rows []stats.AggregateRow, groupBy, filterDesc, ts string) string {
	var b strings.Builder
	b.WriteString("LLM API Proxy - Request Log Summary\r\n")
	b.WriteString("Generated," + ts + "\r\n")
	b.WriteString("Filters," + csvEsc(filterDesc) + "\r\n")
	b.WriteString("Group By," + groupBy + "\r\n")
	b.WriteString("\r\n")

	b.WriteString("Name,Requests,Prompt Tokens,Completion Tokens,Total Tokens,Cached Tokens,Reasoning Tokens,Errors,Error %,Avg Latency (ms),P50 (ms),P90 (ms),P99 (ms),Stream,Non-Stream,Avg TTFT (ms),Avg Gen (ms),Avg TPS\r\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("%s,%d,%d,%d,%d,%d,%d,%d,%.1f,%d,%d,%d,%d,%d,%d,%d,%d,%.1f\r\n",
			csvEsc(r.Name), r.Requests, r.PromptTokens, r.CompletionTokens,
			r.TotalTokens, r.CachedTokens, r.ReasoningTokens, r.Errors, r.ErrorPct,
			r.AvgLatMs, r.P50, r.P90, r.P99, r.StreamCount, r.NonStreamCount,
			r.AvgTTFTMs, r.AvgGenerationMs, r.AvgTPS))
	}
	return b.String()
}

func buildLogSummaryMarkdown(rows []stats.AggregateRow, groupBy, filterDesc, ts string) string {
	var b strings.Builder
	b.WriteString("# LLM API Proxy — Request Log Summary\n\n")
	b.WriteString(fmt.Sprintf("**Generated**: %s  \n", ts))
	b.WriteString(fmt.Sprintf("**Filters**: %s  \n", filterDesc))
	b.WriteString(fmt.Sprintf("**Group By**: %s\n\n", groupBy))

	b.WriteString("| Name | Requests | Prompt | Completion | Total | Cached | Reasoning | Errors | Error % | Avg Latency | P50 | P90 | P99 | Stream | Non-Stream | Avg TTFT | Avg Gen | Avg TPS |\n")
	b.WriteString("|------|----------|--------|------------|-------|--------|-----------|--------|---------|-------------|-----|-----|-----|--------|------------|----------|---------|--------|\n")
	for _, r := range rows {
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s | %d | %.1f%% | %dms | %dms | %dms | %dms | %d | %d | %dms | %dms | %.1f |\n",
			r.Name, humanInt(r.Requests), humanInt(r.PromptTokens), humanInt(r.CompletionTokens),
			humanInt(r.TotalTokens), humanInt(r.CachedTokens), humanInt(r.ReasoningTokens),
			r.Errors, r.ErrorPct, r.AvgLatMs, r.P50, r.P90, r.P99,
			r.StreamCount, r.NonStreamCount, r.AvgTTFTMs, r.AvgGenerationMs, r.AvgTPS))
	}
	return b.String()
}

func csvEsc(s string) string {
	if strings.ContainsAny(s, "\",\n\r") {
		return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `"`, `""`), "\r\n", "\n") + `"`
	}
	return s
}

func humanInt(n int) string {
	s := strconv.FormatInt(int64(n), 10)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(c))
	}
	return string(out)
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
	if id == 0 {
		http.Error(w, "detail not available for in-memory records", http.StatusNotFound)
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
	attempts, _ := u.store.AttemptsForRequest(id)
	data := struct {
		*stats.Record
		Attempts []stats.Attempt
	}{rec, attempts}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "request_detail.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
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

// ToggleDisabledModel adds or removes a model from a backend's disabled_models list.
func (u *UI) ToggleDisabledModel(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Backend  string `json:"backend"`
		Model    string `json:"model"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Backend == "" || payload.Model == "" {
		http.Error(w, "backend and model are required", http.StatusBadRequest)
		return
	}
	if err := u.cfgMgr.ToggleDisabledModel(payload.Backend, payload.Model, payload.Disabled); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// BulkToggleDisabledModels enables or disables all models on a backend in one call.
func (u *UI) BulkToggleDisabledModels(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Backend  string   `json:"backend"`
		Models   []string `json:"models"`
		Disabled bool     `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Backend == "" {
		http.Error(w, "backend is required", http.StatusBadRequest)
		return
	}
	var models []string
	if payload.Disabled {
		models = payload.Models
	}
	if err := u.cfgMgr.ReplaceDisabledModels(payload.Backend, models); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SetModelAlias sets or clears a model alias for a backend. When alias is
// non-empty the model is aliased to that name in the model index; when empty
// the alias is removed.
func (u *UI) SetModelAlias(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Backend string `json:"backend"`
		Model   string `json:"model"`
		Alias   string `json:"alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if payload.Backend == "" || payload.Model == "" {
		http.Error(w, "backend and model are required", http.StatusBadRequest)
		return
	}
	if err := u.cfgMgr.SetModelAlias(payload.Backend, payload.Model, payload.Alias); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	go u.registry.RebuildIndex()
	w.WriteHeader(http.StatusNoContent)
}

// SwitchBackendType changes a backend's type between openai and anthropic,
// updating its base URL and API key. Only openai ↔ anthropic is supported.
func (u *UI) SwitchBackendType(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/backends?msg=Failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	name := r.FormValue("name")
	newType := r.FormValue("type")
	baseURL := r.FormValue("base_url")
	apiKey := r.FormValue("api_key")

	redirectTo := r.FormValue("redirect")
	if redirectTo == "" {
		redirectTo = "/ui/backends"
	}

	if err := u.cfgMgr.SwitchBackendType(name, newType, baseURL, apiKey); err != nil {
		http.Redirect(w, r, redirectTo+"?msg=Error:"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, redirectTo+"?msg=Backend+"+name+"+switched+to+"+newType+".", http.StatusSeeOther)
}

// AddBackendPage adds a new backend from the Models page form.
// isAJAX reports whether the request was sent as an AJAX call (fetch with X-Requested-With header).
func isAJAX(r *http.Request) bool {
	return r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}

func (u *UI) AddBackendPage(w http.ResponseWriter, r *http.Request) {
	ajax := isAJAX(r)

	sendErr := func(msg string) {
		if ajax {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg})
		} else {
			http.Redirect(w, r, "/ui/backends?msg=Error:+"+strings.ReplaceAll(msg, " ", "+"), http.StatusSeeOther)
		}
	}

	if err := r.ParseForm(); err != nil {
		sendErr("failed to parse form")
		return
	}

	bcType := strings.TrimSpace(r.FormValue("type"))
	name := strings.TrimSpace(r.FormValue("name"))
	baseURL := strings.TrimSpace(r.FormValue("base_url"))
	apiKey := strings.TrimSpace(r.FormValue("api_key"))

	if name == "" {
		sendErr("backend name is required")
		return
	}

	bc := config.BackendConfig{
		Name:       name,
		Type:       bcType,
		BaseURL:    baseURL,
		APIKey:     apiKey,
		CompatMode: r.FormValue("compat_mode"),
	}

	// Apply defaults for known backend types.
	switch bcType {
	case "copilot":
		if bc.BaseURL == "" {
			bc.BaseURL = "https://api.githubcopilot.com"
		}
	case "anthropic":
		if bc.BaseURL == "" {
			sendErr("base URL is required for Anthropic-compatible backends")
			return
		}
		if bc.APIKey == "" {
			sendErr("API key is required for Anthropic-compatible backends")
			return
		}
	case "codex":
		if bc.BaseURL == "" {
			bc.BaseURL = "https://chatgpt.com/backend-api/codex"
		}
		if bc.OAuth == nil {
			bc.OAuth = &config.OAuthConfig{
				Scopes:   []string{"openid", "profile", "email", "offline_access"},
				AuthURL:  "https://auth.openai.com/oauth/authorize",
				TokenURL: "https://auth.openai.com/oauth/token",
			}
		}
	case "gemini":
		if bc.BaseURL == "" {
			bc.BaseURL = "https://cloudcode-pa.googleapis.com/v1internal"
		}
		oauthClientID := r.FormValue("oauth_client_id")
		oauthClientSecret := r.FormValue("oauth_client_secret")
		if oauthClientID == "" || oauthClientSecret == "" {
			sendErr("OAuth client ID and client secret are required for Gemini backends. See config.example.yaml for the public Gemini CLI credentials.")
			return
		}
		if bc.OAuth == nil {
			// ClientID and ClientSecret must be provided by the user via the
			// wizard form. The upstream Gemini CLI credentials are documented
			// in config.example.yaml but are not bundled in source.
			bc.OAuth = &config.OAuthConfig{
				ClientID:     oauthClientID,
				ClientSecret: oauthClientSecret,
				Scopes:       []string{"https://www.googleapis.com/auth/cloud-platform", "https://www.googleapis.com/auth/userinfo.email", "https://www.googleapis.com/auth/userinfo.profile"},
				AuthURL:      "https://accounts.google.com/o/oauth2/v2/auth",
				TokenURL:     "https://oauth2.googleapis.com/token",
				RedirectURI:  "http://127.0.0.1:42857/oauth2callback",
			}
		}
	case "ollama":
		if bc.BaseURL == "" {
			bc.BaseURL = "http://localhost:11434"
		}
		if bc.CompatMode == "" {
			bc.CompatMode = "openai"
		}
	case "openai", "":
		bc.Type = "openai"
		if bc.BaseURL == "" {
			sendErr("base URL is required for OpenAI-compatible backends")
			return
		}
		if bc.APIKey == "" {
			sendErr("API key is required for OpenAI-compatible backends")
			return
		}
	default:
		sendErr("unsupported backend type " + bcType)
		return
	}

	if err := u.cfgMgr.AddBackend(bc); err != nil {
		sendErr(err.Error())
		return
	}

	if ajax {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "name": name, "type": bc.Type})
		return
	}
	http.Redirect(w, r, "/ui/backends?msg=Backend+"+name+"+added.", http.StatusSeeOther)
}

// DeleteBackendPage removes a backend from the Models page.
// It accepts an optional wipe_stats form field; when set to "on" it also
// deletes all analytics data associated with the backend.
func (u *UI) DeleteBackendPage(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/backends?msg=Error:+failed+to+parse+form.", http.StatusSeeOther)
		return
	}
	name := r.FormValue("name")
	if name == "" {
		http.Redirect(w, r, "/ui/backends?msg=Error:+backend+name+is+required.", http.StatusSeeOther)
		return
	}

	// Optionally wipe all analytics data for this backend before deleting.
	if r.FormValue("wipe_stats") == "on" {
		n, err := u.collector.DeleteFiltered(stats.StatsFilter{Backend: name})
		if err != nil {
			log.Printf("warning: failed to wipe stats for backend %s: %v", name, err)
		} else {
			log.Printf("wiped %d stats records for backend %s", n, name)
		}
	}

	if err := u.cfgMgr.DeleteBackend(name); err != nil {
		http.Redirect(w, r, "/ui/backends?msg=Error:"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	u.registry.CleanupBackend(name)
	msg := "Backend+" + name + "+deleted."
	if r.FormValue("wipe_stats") == "on" {
		msg = "Backend+" + name + "+deleted+and+analytics+wiped."
	}
	http.Redirect(w, r, "/ui/backends?msg="+msg, http.StatusSeeOther)
}

// WipeAnalytics deletes analytics records matching the given filters.
// Accepts form fields: backend, model (all optional).
// When no filters are provided it acts like "Clear All Stats".
func (u *UI) WipeAnalytics(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/ui/analytics?msg=Error:+failed+to+parse+form.", http.StatusSeeOther)
		return
	}

	var f stats.StatsFilter
	f.Backend = r.FormValue("backend")
	f.Model = r.FormValue("model")

	n, err := u.collector.DeleteFiltered(f)
	if err != nil {
		http.Redirect(w, r, "/ui/analytics?msg=Error:"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	msg := fmt.Sprintf("Deleted+%d+records.", n)
	http.Redirect(w, r, "/ui/analytics?msg="+msg, http.StatusSeeOther)
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
		dur, _ := parseWindowParam(w)
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

// OAuthCheckStatus proactively checks and refreshes the OAuth status for a
// specific backend. Unlike OAuthStatus which only reads cached state, this
// handler triggers a token refresh/re-exchange for backends that implement
// OAuthStatusRefresher and returns only the requested backend fragment.
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

	status, ok := u.registry.OAuthStatus(backendName)
	if !ok {
		http.Error(w, fmt.Sprintf("backend %q does not expose oauth status", backendName), http.StatusNotFound)
		return
	}

	view := r.URL.Query().Get("view")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var err error
	switch view {
	case "models":
		data := modelOAuthCardData{Name: status.BackendName, Type: status.BackendType, OAuth: status}
		err = templates.ExecuteTemplate(w, "models_oauth_card", data)
	default:
		err = templates.ExecuteTemplate(w, "oauth_status_card", status)
	}
	if err != nil {
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
		http.Redirect(w, r, "/ui/backends?msg=OAuth+authentication+failed:+"+errParam, http.StatusSeeOther)
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
		http.Redirect(w, r, "/ui/backends?msg=OAuth+callback+failed:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	log.Printf("oauth: successfully authenticated backend %s", backendName)
	http.Redirect(w, r, "/ui/backends?msg="+backendName+"+authentication+successful!", http.StatusSeeOther)
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

// OAuthDeviceCodeInfo returns device code login info as JSON for the given backend.
// This is used by the Add Backend wizard to show the device code inline in the modal.
func (u *UI) OAuthDeviceCodeInfo(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	if backendName == "" {
		http.Error(w, "backend parameter is required", http.StatusBadRequest)
		return
	}

	b := u.registry.Get(backendName)
	if b == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("backend %q not found", backendName)})
		return
	}

	deviceHandler, ok := b.(backend.OAuthDeviceCodeLoginHandler)
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": fmt.Sprintf("backend %q does not support device code login", backendName)})
		return
	}

	authURL, _, err := deviceHandler.InitiateDeviceCodeLogin()
	if err != nil {
		log.Printf("oauth device code info error for %s: %v", backendName, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": err.Error()})
		return
	}

	var deviceCodeInfo backend.DeviceCodeLoginInfo
	if err := json.Unmarshal([]byte(authURL), &deviceCodeInfo); err != nil || deviceCodeInfo.UserCode == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "backend does not support device code flow"})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":               true,
		"user_code":        deviceCodeInfo.UserCode,
		"verification_uri": deviceCodeInfo.VerificationURI,
		"expires_in":       deviceCodeInfo.ExpiresIn,
	})
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
		http.Redirect(w, r, "/ui/backends?msg=Error:+"+strings.ReplaceAll(err.Error(), " ", "+"), http.StatusSeeOther)
		return
	}

	log.Printf("oauth: disconnected backend %s", backendName)
	http.Redirect(w, r, "/ui/backends?msg="+backendName+"+disconnected+successfully.", http.StatusSeeOther)
}

// RoutingConfigJSON returns the current routing configuration as JSON (used by
// the models page modal to merge per-model changes without clobbering global state).
func (u *UI) RoutingConfigJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(u.cfgMgr.Get().Routing)
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
		log.Error().Err(err).Msg("routing: fallbacks query error")
		http.Error(w, "query error", http.StatusInternalServerError)
		return
	}
	if records == nil {
		records = []stats.Record{}
	}

	// When attempts=1, load per-request attempt traces so the drilldown can show
	// which specific backend failed and why (not just the winning request's error).
	loadAttempts := r.URL.Query().Get("attempts") == "1"

	type recordWithAttempts struct {
		stats.Record
		Attempts []stats.Attempt `json:"attempts,omitempty"`
	}

	items := make([]recordWithAttempts, len(records))
	for i := range records {
		items[i] = recordWithAttempts{Record: records[i]}
		if loadAttempts && u.store != nil {
			if attempts, aErr := u.store.AttemptsForRequest(records[i].ID); aErr == nil && len(attempts) > 0 {
				items[i].Attempts = attempts
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"items": items}); err != nil {
		log.Error().Err(err).Msg("routing: fallbacks encode error")
	}
}

// IdentityProfiles returns the list of available identity profiles as JSON.
func (u *UI) IdentityProfiles(w http.ResponseWriter, r *http.Request) {
	cfg := u.cfgMgr.Get()
	var custom []identity.Profile
	for _, cp := range cfg.CustomIdentityProfiles {
		custom = append(custom, identity.Profile{
			ID:          cp.ID,
			DisplayName: cp.DisplayName,
			UserAgent:   cp.UserAgent,
			Headers:     cp.Headers,
		})
	}
	all := identity.AllProfiles(custom)

	type profileEntry struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Builtin     bool   `json:"builtin"`
	}
	out := make([]profileEntry, len(all))
	for i, p := range all {
		out[i] = profileEntry{ID: p.ID, DisplayName: p.DisplayName, Builtin: identity.IsBuiltin(p.ID)}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// SetGlobalIdentityProfile sets the global identity profile.
func (u *UI) SetGlobalIdentityProfile(w http.ResponseWriter, r *http.Request) {
	// Parse both url-encoded and multipart form bodies.
	if err := r.ParseForm(); err != nil {
		log.Warn().Err(err).Msg("invalid global identity profile form")
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.FormValue("profile") == "" && r.Header.Get("Content-Type") != "" {
		_ = r.ParseMultipartForm(32 << 20)
	}
	profileID := r.FormValue("profile")
	currentProfile := ""
	if cfg := u.cfgMgr.Get(); cfg != nil {
		currentProfile = cfg.IdentityProfile
	}
	log.Debug().
		Str("current_profile", currentProfile).
		Str("requested_profile", profileID).
		Bool("ajax", isAJAX(r)).
		Msg("received global identity profile update request")
	if err := u.cfgMgr.SetGlobalIdentityProfile(profileID); err != nil {
		log.Error().Err(err).
			Str("current_profile", currentProfile).
			Str("requested_profile", profileID).
			Msg("failed to update global identity profile")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadedProfile := ""
	if cfg := u.cfgMgr.Get(); cfg != nil {
		reloadedProfile = cfg.IdentityProfile
	}
	log.Info().
		Str("requested_profile", profileID).
		Str("reloaded_profile", reloadedProfile).
		Bool("ajax", isAJAX(r)).
		Msg("updated global identity profile")
	if isAJAX(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/ui/backends?msg=Global+identity+profile+set+to+"+profileID, http.StatusSeeOther)
}

// SetBackendIdentityProfile sets the identity profile for a specific backend.
func (u *UI) SetBackendIdentityProfile(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "name")
	if backendName == "" {
		log.Warn().Msg("missing backend name for identity profile update")
		http.Error(w, "backend name required", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		log.Warn().Err(err).Str("backend", backendName).Msg("invalid backend identity profile form")
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.FormValue("profile") == "" && r.Header.Get("Content-Type") != "" {
		_ = r.ParseMultipartForm(32 << 20)
	}
	profileID := r.FormValue("profile")
	currentProfile := ""
	if cfg := u.cfgMgr.Get(); cfg != nil {
		for _, backend := range cfg.Backends {
			if backend.Name == backendName {
				currentProfile = backend.IdentityProfile
				break
			}
		}
	}
	log.Debug().
		Str("backend", backendName).
		Str("current_profile", currentProfile).
		Str("requested_profile", profileID).
		Bool("ajax", isAJAX(r)).
		Msg("received backend identity profile update request")
	if err := u.cfgMgr.SetBackendIdentityProfile(backendName, profileID); err != nil {
		log.Error().Err(err).
			Str("backend", backendName).
			Str("current_profile", currentProfile).
			Str("requested_profile", profileID).
			Msg("failed to update backend identity profile")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	reloadedProfile := ""
	if cfg := u.cfgMgr.Get(); cfg != nil {
		for _, backend := range cfg.Backends {
			if backend.Name == backendName {
				reloadedProfile = backend.IdentityProfile
				break
			}
		}
	}
	log.Info().
		Str("backend", backendName).
		Str("requested_profile", profileID).
		Str("reloaded_profile", reloadedProfile).
		Bool("ajax", isAJAX(r)).
		Msg("updated backend identity profile")
	if isAJAX(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/ui/backends?msg=Identity+profile+for+"+backendName+"+set+to+"+profileID, http.StatusSeeOther)
}

// ── Circuit Breaker Handlers ────────────────────────────

// CircuitStates returns all circuit breaker states as JSON.
func (u *UI) CircuitStates(w http.ResponseWriter, r *http.Request) {
	if u.circuit == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]struct{}{})
		return
	}
	states := u.circuit.AllStates()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(states)
}

// CircuitCard renders the circuit breaker health cards as an HTML fragment.
func (u *UI) CircuitCard(w http.ResponseWriter, r *http.Request) {
	if u.circuit == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}

	// Get all backend names from config.
	cfg := u.cfgMgr.Get()
	type backendCircuit struct {
		Name  string
		State CircuitBreakerState
	}
	var backends []backendCircuit
	for _, bc := range cfg.Backends {
		if bc.Enabled != nil && !*bc.Enabled {
			continue
		}
		backends = append(backends, backendCircuit{
			Name:  bc.Name,
			State: u.circuit.State(bc.Name),
		})
	}

	data := map[string]any{
		"Backends": backends,
		"Enabled":  u.circuit.Enabled(),
	}
	injectAuth(r, data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "circuit_card.html", data); err != nil {
		log.Error().Err(err).Msg("template error")
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// CircuitReset resets a specific backend's circuit breaker.
func (u *UI) CircuitReset(w http.ResponseWriter, r *http.Request) {
	if u.circuit == nil {
		http.Error(w, "circuit breaker not available", http.StatusServiceUnavailable)
		return
	}
	backendName := chi.URLParam(r, "name")
	if backendName == "" {
		http.Error(w, "backend name required", http.StatusBadRequest)
		return
	}
	u.circuit.Reset(backendName)
	log.Info().Str("backend", backendName).Msg("circuit breaker reset via UI")

	// If a redirect param is provided, redirect (e.g. from models page dropdown).
	if redir := r.URL.Query().Get("redirect"); redir != "" {
		http.Redirect(w, r, redir+"?msg=Circuit+breaker+reset+for+"+backendName, http.StatusSeeOther)
		return
	}

	// Default: return HTMX fragment for dashboard.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.ExecuteTemplate(w, "circuit_card.html", map[string]any{
		"Backends": func() []struct {
			Name  string
			State CircuitBreakerState
		} {
			cfg := u.cfgMgr.Get()
			var backends []struct {
				Name  string
				State CircuitBreakerState
			}
			for _, bc := range cfg.Backends {
				if bc.Enabled != nil && !*bc.Enabled {
					continue
				}
				backends = append(backends, struct {
					Name  string
					State CircuitBreakerState
				}{Name: bc.Name, State: u.circuit.State(bc.Name)})
			}
			return backends
		}(),
		"Enabled": u.circuit.Enabled(),
	})
}

// CircuitResetAll resets all circuit breakers.
func (u *UI) CircuitResetAll(w http.ResponseWriter, r *http.Request) {
	if u.circuit == nil {
		http.Error(w, "circuit breaker not available", http.StatusServiceUnavailable)
		return
	}
	u.circuit.ResetAll()
	log.Info().Msg("all circuit breakers reset via UI")

	// Redirect back to the referer or dashboard.
	redirect := r.Header.Get("Referer")
	if redirect == "" {
		redirect = "/ui/"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// CircuitConfigUpdate updates the circuit breaker configuration.
func (u *UI) CircuitConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if u.circuit == nil {
		http.Error(w, "circuit breaker not available", http.StatusServiceUnavailable)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form data", http.StatusBadRequest)
		return
	}

	enabled := r.FormValue("circuit_enabled") == "on"
	threshold := 3
	if v := r.FormValue("circuit_threshold"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			threshold = n
		}
	}
	cooldown := 300
	if v := r.FormValue("circuit_cooldown"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cooldown = n
		}
	}

	// Persist to config.
	cfg := u.cfgMgr.Get()
	cfg.Routing.CircuitBreaker = config.CircuitBreakerConfig{}
	trueVal := true
	if enabled {
		cfg.Routing.CircuitBreaker.Enabled = &trueVal
	} else {
		falseVal := false
		cfg.Routing.CircuitBreaker.Enabled = &falseVal
	}
	cfg.Routing.CircuitBreaker.Threshold = threshold
	cfg.Routing.CircuitBreaker.CooldownSec = cooldown

	data, err := yaml.Marshal(cfg)
	if err != nil {
		http.Error(w, "failed to marshal config", http.StatusInternalServerError)
		return
	}
	if err := u.cfgMgr.SaveRaw(data); err != nil {
		http.Error(w, "failed to save config: "+err.Error(), http.StatusInternalServerError)
		return
	}

	log.Info().Bool("enabled", enabled).Int("threshold", threshold).Int("cooldown", cooldown).Msg("circuit breaker config updated via UI")
	http.Redirect(w, r, "/ui/settings?msg=Circuit+breaker+config+updated", http.StatusSeeOther)
}

// ── Ollama Management ──────────────────────────────────────────

// ollamaBackend resolves the backend by name and asserts it's an Ollama backend.
// Returns nil if not found or not Ollama.
func (u *UI) ollamaBackend(name string) *backend.OllamaBackend {
	b := u.registry.Get(name)
	if b == nil {
		return nil
	}
	ob, ok := b.(*backend.OllamaBackend)
	if !ok {
		return nil
	}
	return ob
}

// OllamaPullModel starts pulling a model on an Ollama backend.
// POST /ui/ollama/{backend}/pull
func (u *UI) OllamaPullModel(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		http.Error(w, "model field required", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	ch, err := mgr.PullModel(r.Context(), req.Model)
	if err != nil {
		log.Error().Err(err).Str("backend", backendName).Str("model", req.Model).Msg("ollama pull failed to start")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Stream progress as NDJSON if AJAX, else consume silently.
	if isAJAX(r) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Transfer-Encoding", "chunked")
		for progress := range ch {
			data, _ := json.Marshal(progress)
			w.Write(data)
			w.Write([]byte("\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			if progress.Done {
				break
			}
		}
		return
	}

	// Non-AJAX: consume in background, redirect.
	go func() {
		for range ch {
		}
	}()

	redirect := r.URL.Query().Get("redirect")
	if redirect == "" {
		redirect = "/ui/backends"
	}
	http.Redirect(w, r, redirect+"?msg=Pulling+"+req.Model+"+on+"+backendName, http.StatusSeeOther)
}

// OllamaCancelPull cancels an active model pull.
// POST /ui/ollama/{backend}/cancel
func (u *UI) OllamaCancelPull(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Model == "" {
		http.Error(w, "model field required", http.StatusBadRequest)
		return
	}

	cancelled := ob.CancelPullByModel(req.Model)
	log.Info().Str("backend", backendName).Str("model", req.Model).Bool("cancelled", cancelled).Msg("ollama pull cancel requested")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "cancelled": cancelled})
}

// OllamaPullStatus returns the status of active/recent pulls for a backend.
// Reads directly from the long-lived backend instance so it survives page reloads.
// GET /ui/ollama/{backend}/pulls
func (u *UI) OllamaPullStatus(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	statuses := ob.ActivePulls()
	if statuses == nil {
		statuses = []backend.OllamaPullStatus{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(statuses)
}

// OllamaDeleteModel removes a model from an Ollama backend.
// DELETE /ui/ollama/{backend}/models/{model}
// Also accepts POST with JSON body {"model":"name"} for names with special chars.
func (u *UI) OllamaDeleteModel(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	// Model name can come from URL param or JSON body (preferred for special chars).
	modelName := chi.URLParam(r, "model")
	if r.Method == http.MethodPost {
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Model != "" {
			modelName = req.Model
		}
	}
	if modelName == "" {
		http.Error(w, "model name required", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	if err := mgr.DeleteModel(r.Context(), modelName); err != nil {
		log.Error().Err(err).Str("backend", backendName).Str("model", modelName).Msg("ollama delete failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Info().Str("backend", backendName).Str("model", modelName).Msg("ollama model deleted")
	if isAJAX(r) || r.Method == http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
		return
	}
	redirect := r.URL.Query().Get("redirect")
	if redirect == "" {
		redirect = "/ui/backends"
	}
	http.Redirect(w, r, redirect+"?msg=Model+"+modelName+"+deleted+from+"+backendName, http.StatusSeeOther)
}

// OllamaShowModel returns detailed information about a model.
// GET /ui/ollama/{backend}/models/{model}
func (u *UI) OllamaShowModel(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	modelName := chi.URLParam(r, "model")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	info, err := mgr.ShowModelDetails(r.Context(), modelName)
	if err != nil {
		log.Error().Err(err).Str("backend", backendName).Str("model", modelName).Msg("ollama show failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// OllamaListRunning returns models currently loaded in memory.
// GET /ui/ollama/{backend}/ps
func (u *UI) OllamaListRunning(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	models, err := mgr.ListRunningModels(r.Context())
	if err != nil {
		log.Error().Err(err).Str("backend", backendName).Msg("ollama ps failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(models)
}

// OllamaWhoami returns the signin status of an Ollama backend.
// GET /ui/ollama/{backend}/account
func (u *UI) OllamaWhoami(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	result, err := mgr.Whoami(r.Context())
	if err != nil {
		log.Error().Err(err).Str("backend", backendName).Msg("ollama whoami failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := map[string]any{
		"signed_in":  result.Name != "",
		"name":       result.Name,
		"signin_url": result.SigninURL,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// OllamaSignout signs out from ollama.com via the local Ollama server.
// POST /ui/ollama/{backend}/signout
func (u *UI) OllamaSignout(w http.ResponseWriter, r *http.Request) {
	backendName := chi.URLParam(r, "backend")
	ob := u.ollamaBackend(backendName)
	if ob == nil {
		http.Error(w, "not an ollama backend", http.StatusBadRequest)
		return
	}

	mgr := backend.NewOllamaManager(ob)
	if err := mgr.Signout(r.Context()); err != nil {
		log.Error().Err(err).Str("backend", backendName).Msg("ollama signout failed")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Info().Str("backend", backendName).Msg("ollama signed out")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
