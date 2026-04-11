package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/chat"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/logger"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/proxy"
	"github.com/menno/llmapiproxy/internal/stats"
	"github.com/menno/llmapiproxy/internal/web"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	logLevel := flag.String("log-level", "info", "Log level: debug, info, warn, error")
	logJSON := flag.Bool("log-json", false, "Output structured JSON logs")
	flag.Parse()

	logger.Init(*logLevel, *logJSON)

	cfgMgr, err := config.NewManager(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
	}
	defer cfgMgr.Close()
	cfg := cfgMgr.Get()
	log.Info().Str("path", *configPath).Int("backends", len(cfg.Backends)).Msg("loaded config")

	if err := cfgMgr.WatchFile(); err != nil {
		log.Warn().Err(err).Msg("config file watching disabled")
	} else {
		log.Info().Str("path", *configPath).Msg("watching config file for changes")
	}

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfg)

	cfgMgr.OnChange(func(newCfg *config.Config) {
		registry.LoadFromConfig(newCfg)
		log.Info().Int("backends", len(newCfg.Backends)).Msg("backends reloaded")
	})

	collector := stats.NewCollector(10000)

	var store *stats.Store
	if !cfg.Server.DisableStats {
		var err error
		store, err = stats.OpenStore(cfg.Server.StatsPath, collector)
		if err != nil {
			log.Fatal().Err(err).Str("path", cfg.Server.StatsPath).Msg("failed to open stats database")
		}
		collector.SetStore(store)
		log.Info().Str("path", cfg.Server.StatsPath).Msg("stats database opened")
	} else {
		log.Info().Msg("stats logging disabled (disable_stats: true)")
	}

	proxyHandler := proxy.NewHandler(registry, collector, cfgMgr)

	chatStore, err := chat.OpenChatStore(cfg.Server.ChatDBPath)
	if err != nil {
		log.Fatal().Err(err).Str("path", cfg.Server.ChatDBPath).Msg("failed to open chat database")
	}
	log.Info().Str("path", cfg.Server.ChatDBPath).Msg("chat database opened")

	appBaseURL := oauth.DeriveLocalServerBaseURL(cfg.Server.Listen)

	ui := web.NewUI(cfgMgr, collector, registry, store, chatStore)

	r := chi.NewRouter()
	r.Use(chiMiddleware.RealIP)
	r.Use(chiMiddleware.Recoverer)
	r.Use(chiMiddleware.RequestID)

	r.Route("/v1", func(r chi.Router) {
		r.Use(proxy.AuthMiddleware(cfgMgr))
		r.Post("/chat/completions", proxyHandler.ChatCompletions)
		r.Post("/responses", proxyHandler.Responses)
		r.Get("/models", proxyHandler.ListModels)
	})

	r.Route("/ui", func(r chi.Router) {
		r.Get("/", ui.Dashboard)
		r.Get("/dashboard/data", ui.DashboardData)
		r.Get("/stats", ui.StatsFragment)
		r.Get("/models", ui.ModelsPage)
		r.Get("/backends/{name}/models", ui.BackendModels)
		r.Post("/backends/{name}/refresh-models", ui.RefreshBackendModels)
		r.Get("/config", http.RedirectHandler("/ui/settings", http.StatusSeeOther).ServeHTTP)
		r.Post("/config/save", ui.SaveConfig)
		r.Post("/settings/model-cache-ttl", ui.UpdateModelCacheTTL)
		r.Get("/settings", ui.SettingsPage)
		r.Get("/playground", ui.PlaygroundPage)
		r.Get("/playground/models", ui.PlaygroundModels)

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
		r.Post("/settings/clear-stats", ui.ClearStats)
		r.Post("/settings/toggle-stats", ui.ToggleStats)
		r.Post("/settings/keys/add", ui.AddAPIKey)
		r.Post("/settings/keys/delete", ui.DeleteAPIKey)
		r.Post("/settings/backends/toggle", ui.ToggleBackend)
		r.Get("/stats/cards", ui.StatsCards)
		r.Get("/stats/detail", ui.RequestDetail)
		r.Get("/analytics", ui.AnalyticsPage)
		r.Get("/analytics/data", ui.AnalyticsData)
		r.Get("/routing", ui.RoutingPage)
		r.Get("/routing/data", ui.RoutingData)
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
		r.Get("/oauth/callback/{backend}", ui.OAuthCallback)
		r.Post("/oauth/disconnect/{backend}", ui.OAuthDisconnect)
		r.Post("/oauth/check-status/{backend}", ui.OAuthCheckStatus)

		staticSub, err := fs.Sub(web.StaticFS(), "static")
		if err != nil {
			log.Fatal().Err(err).Msg("failed to create static sub-filesystem")
		}
		r.Handle("/static/*", http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticSub))))
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
	if store != nil {
		store.Close()
	}
	chatStore.Close()
	log.Info().Msg("server stopped")
}
