package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/chat"
	"github.com/menno/llmapiproxy/internal/chatv2"
	"github.com/menno/llmapiproxy/internal/circuit"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/logger"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/proxy"
	"github.com/menno/llmapiproxy/internal/stats"
	"github.com/menno/llmapiproxy/internal/users"
	"github.com/menno/llmapiproxy/internal/web"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the LLM API Proxy server",
	Long:  "Start the reverse proxy server that unifies multiple LLM providers behind a single OpenAI-compatible endpoint.",
	RunE: func(cmd *cobra.Command, args []string) error {
		configPath, _ := cmd.Flags().GetString("config")
		logLevel, _ := cmd.Flags().GetString("log-level")
		logJSON, _ := cmd.Flags().GetBool("log-json")

		logger.Init(logLevel, logJSON)

		cfgMgr, err := config.NewManager(configPath)
		if err != nil {
			return fmt.Errorf("failed to load config: %w", err)
		}
		defer cfgMgr.Close()
		cfg := cfgMgr.Get()
		log.Info().Str("path", configPath).Int("backends", len(cfg.Backends)).Msg("loaded config")

		// Warn if the web UI is publicly accessible without authentication.
		if !cfg.Server.WebAuth {
			host := cfg.Server.Host
			if host != "" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
				log.Warn().
					Str("host", host).
					Msg("web UI is publicly accessible without authentication — consider enabling server.web_auth or binding to localhost")
			} else if host == "" {
				log.Warn().
					Msg("web UI is accessible on all interfaces without authentication — consider enabling server.web_auth")
			}
		}

		if err := cfgMgr.WatchFile(); err != nil {
			log.Warn().Err(err).Msg("config file watching disabled")
		} else {
			log.Info().Str("path", configPath).Msg("watching config file for changes")
		}

		registry := backend.NewRegistry()
		registry.LoadFromConfig(cfg)

		// Circuit breaker for 429 rate-limit protection.
		circuitMgr := circuit.NewManager(routingCircuitConfig(cfg))
		for _, bc := range cfg.Backends {
			if bc.Enabled == nil || *bc.Enabled {
				circuitMgr.EnsureBackend(bc.Name)
			}
		}

		cfgMgr.OnChange(func(newCfg *config.Config) {
			registry.LoadFromConfig(newCfg)
			circuitMgr.UpdateConfig(routingCircuitConfig(newCfg))
			for _, bc := range newCfg.Backends {
				if bc.Enabled == nil || *bc.Enabled {
					circuitMgr.EnsureBackend(bc.Name)
				}
			}
			log.Info().Int("backends", len(newCfg.Backends)).Msg("backends reloaded")
		})

		// Ensure the data directory exists so database files and tokens can be created.
		if err := os.MkdirAll("data/tokens", 0700); err != nil {
			return fmt.Errorf("failed to create data directory: %w", err)
		}

		collector := stats.NewCollector(10000)

		var store *stats.Store
		if !cfg.Server.DisableStats {
			store, err = stats.OpenStore(cfg.Server.StatsPath, collector)
			if err != nil {
				return fmt.Errorf("failed to open stats database: %w", err)
			}
			collector.SetStore(store)
			log.Info().Str("path", cfg.Server.StatsPath).Msg("stats database opened")
		} else {
			log.Info().Msg("stats logging disabled (disable_stats: true)")
		}

		proxyHandler := proxy.NewHandler(registry, collector, cfgMgr, circuitMgr)

		chatStore, err := chat.OpenChatStore(cfg.Server.ChatDBPath)
		if err != nil {
			return fmt.Errorf("failed to open chat database: %w", err)
		}
		log.Info().Str("path", cfg.Server.ChatDBPath).Msg("chat database opened")

		// Open chatv2 store (separate database file).
		chatv2DBPath := "data/chatv2.db"
		chatv2Store, err := chatv2.OpenStore(chatv2DBPath)
		if err != nil {
			return fmt.Errorf("failed to open chatv2 database: %w", err)
		}
		defer chatv2Store.Close()
		log.Info().Str("path", chatv2DBPath).Msg("chatv2 database opened")

		appBaseURL := oauth.BaseURL(cfg.Server.Domain, cfg.Server.Listen)

		web.SetVersion(version)

		// Open user store and generate session secret when web auth is enabled.
		var userStore *users.UserStore
		var sessionSecret []byte
		if cfg.Server.WebAuth {
			if err := os.MkdirAll(filepath.Dir(cfg.Server.UsersDBPath), 0700); err != nil {
				return fmt.Errorf("failed to create users database directory: %w", err)
			}
			userStore, err = users.OpenUserStore(cfg.Server.UsersDBPath)
			if err != nil {
				return fmt.Errorf("failed to open users database: %w", err)
			}
			defer userStore.Close()
			log.Info().Str("path", cfg.Server.UsersDBPath).Msg("users database opened")

			if cfg.Server.WebAuthSecret != "" {
				sessionSecret = []byte(cfg.Server.WebAuthSecret)
			} else {
				sessionSecret = users.GenerateSessionSecret()
			}
			log.Info().Msg("web UI authentication enabled")
		}

		ui := web.NewUI(cfgMgr, collector, registry, store, chatStore, userStore, sessionSecret, circuitMgrAdapter{mgr: circuitMgr})

		// Create the chatv2 handler.
		chatv2Handler := web.NewChatV2Handler(chatv2Store, cfgMgr, registry)

		r := chi.NewRouter()
		r.Use(chiMiddleware.RealIP)
		r.Use(chiMiddleware.Recoverer)
		r.Use(chiMiddleware.RequestID)

		r.Route("/v1", func(r chi.Router) {
			r.Use(proxy.AuthMiddleware(cfgMgr))
			r.Post("/chat/completions", proxyHandler.ChatCompletions)
			r.Post("/messages", proxyHandler.AnthropicMessages)
			r.Post("/responses", proxyHandler.Responses)
			r.Get("/models", proxyHandler.ListModels)
		})

		// Static assets (always accessible, no auth).
		staticSub, err := fs.Sub(web.StaticFS(), "static")
		if err != nil {
			return fmt.Errorf("failed to create static sub-filesystem: %w", err)
		}
		r.Handle("/ui/static/*", http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticSub))))

		// PWA files
		r.Get("/ui/manifest.json", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFile(w, r, "internal/web/manifest.json")
		})
		r.Get("/ui/sw.js", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript")
			http.ServeFile(w, r, "internal/web/sw.js")
		})

		// Auth-exempt routes: login, setup, logout.
		r.Get("/ui/login", ui.LoginPage)
		r.Post("/ui/login", ui.LoginPost)
		r.Get("/ui/setup", ui.SetupPage)
		r.Post("/ui/setup", ui.SetupPost)
		r.Post("/ui/logout", ui.LogoutPost)

		// Protected /ui/* routes — apply auth middleware when web auth is enabled.
		r.Route("/ui", func(r chi.Router) {
			if cfg.Server.WebAuth {
				r.Use(users.AuthMiddleware(userStore, sessionSecret))
			}

			// Dashboard & stats
			r.Get("/", ui.Dashboard)
			r.Get("/dashboard/data", ui.DashboardData)
			r.Get("/stats", ui.StatsFragment)
			r.Get("/stats/tps-histogram", ui.TPSHistogram)
			r.Get("/models", ui.ModelsPage)
			r.Get("/backends", ui.BackendsPage)
			r.Get("/backends/{name}/models", ui.BackendModels)
			r.Get("/backends/{name}/upstream-models", ui.BackendUpstreamModels)
			r.Post("/backends/{name}/refresh-models", ui.RefreshBackendModels)
			r.Get("/config", http.RedirectHandler("/ui/settings", http.StatusSeeOther).ServeHTTP)
			r.Post("/config/save", ui.SaveConfig)
			r.Post("/settings/model-cache-ttl", ui.UpdateModelCacheTTL)
			r.Get("/settings", ui.SettingsPage)

			// Chat API
			r.Get("/chat", ui.ChatPage)
			r.Get("/chat/models", ui.ChatModels)
			r.Get("/chat/sessions", ui.ChatListSessions)
			r.Post("/chat/sessions", ui.ChatCreateSession)
			r.Get("/chat/sessions/{id}", ui.ChatGetSession)
			r.Put("/chat/sessions/{id}", ui.ChatUpdateSession)
			r.Delete("/chat/sessions/{id}", ui.ChatDeleteSession)
			r.Delete("/chat/sessions", ui.ChatDeleteAllSessions)
			r.Get("/chat/sessions/{id}/messages", ui.ChatListMessages)
			r.Post("/chat/sessions/{id}/messages", ui.ChatSaveMessage)
			r.Post("/chat/sessions/{id}/title", ui.ChatGenerateTitle)
			r.Put("/chat/title-model", ui.ChatSetTitleModel)
			r.Put("/chat/default-model", ui.ChatSetDefaultModel)

			// ChatV2 (Chat Beta) routes
			r.Get("/chatv2", chatv2Handler.ChatV2Page)
			r.Get("/chatv2/models", chatv2Handler.ChatV2Models)
			r.Get("/chatv2/sessions", chatv2Handler.ChatV2ListSessions)
			r.Post("/chatv2/sessions", chatv2Handler.ChatV2CreateSession)
			r.Get("/chatv2/sessions/{id}", chatv2Handler.ChatV2GetSession)
			r.Put("/chatv2/sessions/{id}", chatv2Handler.ChatV2UpdateSession)
			r.Delete("/chatv2/sessions/{id}", chatv2Handler.ChatV2DeleteSession)
			r.Delete("/chatv2/sessions", chatv2Handler.ChatV2DeleteAllSessions)
			r.Get("/chatv2/sessions/{id}/messages", chatv2Handler.ChatV2ListMessages)
			r.Post("/chatv2/sessions/{id}/messages", chatv2Handler.ChatV2SaveMessage)
			r.Post("/chatv2/sessions/{id}/title", chatv2Handler.ChatV2GenerateTitle)
			r.Get("/chatv2/sessions/search", chatv2Handler.ChatV2SearchSessions)
			r.Post("/chatv2/sessions/{id}/export", chatv2Handler.ChatV2ExportSession)
			r.Get("/chatv2/defaults", chatv2Handler.ChatV2GetDefaults)
			r.Put("/chatv2/defaults/{model}", chatv2Handler.ChatV2SetDefaults)
			r.Post("/settings/clear-stats", ui.ClearStats)
			r.Post("/settings/toggle-stats", ui.ToggleStats)
			r.Post("/settings/keys/add", ui.AddAPIKey)
			r.Post("/settings/keys/delete", ui.DeleteAPIKey)
			r.Post("/settings/backends/toggle", ui.ToggleBackend)
			r.Post("/settings/backends/switch-type", ui.SwitchBackendType)
			r.Post("/settings/backends/disabled-model", ui.ToggleDisabledModel)
			r.Post("/settings/backends/bulk-disabled-model", ui.BulkToggleDisabledModels)
			r.Post("/settings/backends/model-alias", ui.SetModelAlias)
			r.Post("/settings/backends/add", ui.AddBackendPage)
			r.Post("/settings/backends/delete", ui.DeleteBackendPage)
			r.Get("/json/identity-profiles", ui.IdentityProfiles)
			r.Post("/identity-profile", ui.SetGlobalIdentityProfile)
			r.Post("/backends/{name}/identity-profile", ui.SetBackendIdentityProfile)
			r.Post("/analytics/wipe", ui.WipeAnalytics)
			r.Get("/stats/cards", ui.StatsCards)
			r.Get("/stats/detail", ui.RequestDetail)
			r.Get("/export/overview", ui.ExportOverview)
			r.Get("/export/log-summary", ui.ExportLogSummary)
			r.Get("/analytics", http.RedirectHandler("/ui/", http.StatusSeeOther).ServeHTTP)
			r.Get("/routing", http.RedirectHandler("/ui/", http.StatusSeeOther).ServeHTTP)
			r.Get("/routing/config", ui.RoutingConfigJSON)
			r.Get("/routing/backend-fallbacks", ui.RoutingBackendFallbacks)
			r.Post("/settings/clients/add", ui.AddClient)
			r.Post("/settings/clients/delete", ui.DeleteClient)
			r.Post("/settings/server", ui.UpdateServerAddr)
			r.Post("/routing/save", ui.SaveRouting)

			// OAuth management endpoints
			r.Get("/oauth/status", ui.OAuthStatus)
			r.Get("/oauth/login/{backend}", ui.OAuthLogin)
			r.Get("/oauth/device-login/{backend}", ui.OAuthDeviceLogin)
			r.Get("/oauth/device-code-info/{backend}", ui.OAuthDeviceCodeInfo)
			r.Get("/oauth/callback/{backend}", ui.OAuthCallback)
			r.Post("/oauth/disconnect/{backend}", ui.OAuthDisconnect)
			r.Post("/oauth/check-status/{backend}", ui.OAuthCheckStatus)

			// Circuit breaker management
			r.Get("/circuit/card", ui.CircuitCard)
			r.Get("/circuit/states", ui.CircuitStates)
			r.Post("/circuit/reset/{name}", ui.CircuitReset)
			r.Post("/circuit/reset-all", ui.CircuitResetAll)
			r.Post("/circuit/config", ui.CircuitConfigUpdate)

			// Ollama management
			r.Post("/ollama/{backend}/pull", ui.OllamaPullModel)
			r.Get("/ollama/{backend}/pulls", ui.OllamaPullStatus)
			r.Post("/ollama/{backend}/cancel", ui.OllamaCancelPull)
			r.Delete("/ollama/{backend}/models/{model}", ui.OllamaDeleteModel)
			r.Post("/ollama/{backend}/models/delete", ui.OllamaDeleteModel)
			r.Get("/ollama/{backend}/models/{model}", ui.OllamaShowModel)
			r.Get("/ollama/{backend}/ps", ui.OllamaListRunning)
			r.Get("/ollama/{backend}/account", ui.OllamaWhoami)
			r.Post("/ollama/{backend}/signout", ui.OllamaSignout)
		})

		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/ui/", http.StatusTemporaryRedirect)
		})

		r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")

			statuses := registry.OAuthStatuses()
			allHealthy := true
			for _, s := range statuses {
				if s.NeedsReauth {
					allHealthy = false
					break
				}
			}

			overallStatus := "ok"
			if !allHealthy {
				overallStatus = "degraded"
			}

			resp := map[string]interface{}{
				"status":   overallStatus,
				"backends": statuses,
			}
			json.NewEncoder(w).Encode(resp)
		})

		srv := &http.Server{
			Addr:              cfg.Server.Listen,
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
		}
		codexLoopbackSrv := &http.Server{
			Addr:              codexLoopbackListenAddr,
			Handler:           newCodexLoopbackHandler(registry, appBaseURL),
			ReadHeaderTimeout: 10 * time.Second,
		}
		geminiLoopbackSrv := &http.Server{
			Addr:              geminiLoopbackListenAddr,
			Handler:           newGeminiLoopbackHandler(registry, appBaseURL),
			ReadHeaderTimeout: 10 * time.Second,
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()

		sighup := make(chan os.Signal, 1)
		signal.Notify(sighup, syscall.SIGHUP)
		go func() {
			for range sighup {
				log.Info().Msg("received SIGHUP, reloading config")
				if err := cfgMgr.Reload(); err != nil {
					log.Error().Err(err).Msg("config reload failed")
				} else {
					log.Info().Msg("config reloaded successfully")
				}
			}
		}()

		go func() {
			displayURL := fmt.Sprintf("http://localhost:%d", cfg.Server.Port)
			log.Info().Str("addr", cfg.Server.Listen).Msg("starting server")
			log.Info().Str("url", displayURL).Msg("  API:       /v1/chat/completions")
			log.Info().Str("url", displayURL).Msg("  Dashboard: /ui/")
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal().Err(err).Msg("server error")
			}
		}()
		go func() {
			log.Printf("starting Codex OAuth loopback callback server on %s", codexLoopbackListenAddr)
			if err := codexLoopbackSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("codex loopback callback server error: %v", err)
			}
		}()
		go func() {
			log.Printf("starting Gemini OAuth loopback callback server on %s", geminiLoopbackListenAddr)
			if err := geminiLoopbackSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Printf("gemini loopback callback server error: %v", err)
			}
		}()

		<-ctx.Done()
		log.Info().Msg("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Fatal().Err(err).Msg("shutdown error")
		}
		if err := codexLoopbackSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("codex loopback shutdown error: %v", err)
		}
		if err := geminiLoopbackSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("gemini loopback shutdown error: %v", err)
		}
		if store != nil {
			store.Close()
		}
		chatStore.Close()
		log.Info().Msg("server stopped")
		return nil
	},
}

func init() {
	serveCmd.Flags().String("config", "data/config.yaml", "Path to configuration file")
	serveCmd.Flags().String("log-level", "info", "Log level: debug, info, warn, error")
	serveCmd.Flags().Bool("log-json", false, "Output structured JSON logs")
}

// routingCircuitConfig converts the config YAML into a circuit.Config with defaults.
func routingCircuitConfig(cfg *config.Config) circuit.Config {
	cb := cfg.Routing.CircuitBreaker
	c := circuit.DefaultConfig()
	if cb.Enabled != nil {
		c.Enabled = *cb.Enabled
	}
	if cb.Threshold > 0 {
		c.Threshold = cb.Threshold
	}
	if cb.CooldownSec > 0 {
		c.Cooldown = cb.CooldownSec
	}
	return c
}

// circuitMgrAdapter adapts circuit.Manager to the web.CircuitManager interface.
type circuitMgrAdapter struct {
	mgr *circuit.Manager
}

func (a circuitMgrAdapter) AllStates() []web.CircuitBreakerState {
	states := a.mgr.AllStates()
	out := make([]web.CircuitBreakerState, len(states))
	for i, s := range states {
		out[i] = web.CircuitBreakerState(s)
	}
	return out
}

func (a circuitMgrAdapter) State(backendName string) web.CircuitBreakerState {
	s := a.mgr.State(backendName)
	return web.CircuitBreakerState(s)
}

func (a circuitMgrAdapter) Reset(backendName string) { a.mgr.Reset(backendName) }
func (a circuitMgrAdapter) ResetAll()                { a.mgr.ResetAll() }
func (a circuitMgrAdapter) Enabled() bool            { return a.mgr.Enabled() }
func (a circuitMgrAdapter) GetConfig() web.CircuitBreakerConfig {
	c := a.mgr.GetConfig()
	return web.CircuitBreakerConfig(c)
}
func (a circuitMgrAdapter) UpdateConfig(enabled bool, threshold int, cooldownSec int) {
	a.mgr.UpdateConfig(circuit.Config{Enabled: enabled, Threshold: threshold, Cooldown: cooldownSec})
}
