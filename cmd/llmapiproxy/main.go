package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chiMiddleware "github.com/go-chi/chi/v5/middleware"

	"github.com/menno/llmapiproxy/internal/backend"
	"github.com/menno/llmapiproxy/internal/config"
	"github.com/menno/llmapiproxy/internal/oauth"
	"github.com/menno/llmapiproxy/internal/proxy"
	"github.com/menno/llmapiproxy/internal/stats"
	"github.com/menno/llmapiproxy/internal/web"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to configuration file")
	flag.Parse()

	cfgMgr, err := config.NewManager(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	cfg := cfgMgr.Get()
	log.Printf("loaded config from %s with %d backends", *configPath, len(cfg.Backends))

	registry := backend.NewRegistry()
	registry.LoadFromConfig(cfg)

	cfgMgr.OnChange(func(newCfg *config.Config) {
		registry.LoadFromConfig(newCfg)
		log.Printf("backends reloaded: %d backends", len(newCfg.Backends))
	})

	collector := stats.NewCollector(10000)

	var store *stats.Store
	if !cfg.Server.DisableStats {
		var err error
		store, err = stats.OpenStore(cfg.Server.StatsPath, collector)
		if err != nil {
			log.Fatalf("failed to open stats database: %v", err)
		}
		collector.SetStore(store)
		log.Printf("stats database: %s", cfg.Server.StatsPath)
	} else {
		log.Printf("stats logging disabled (disable_stats: true)")
	}

	proxyHandler := proxy.NewHandler(registry, collector, cfgMgr)
	ui := web.NewUI(cfgMgr, collector, registry, store)
	appBaseURL := oauth.DeriveLocalServerBaseURL(cfg.Server.Listen)

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
		r.Get("/stats", ui.StatsFragment)
		r.Get("/models", ui.ModelsPage)
		r.Get("/config", http.RedirectHandler("/ui/settings", http.StatusSeeOther).ServeHTTP)
		r.Post("/config/save", ui.SaveConfig)
		r.Get("/settings", ui.SettingsPage)
		r.Get("/playground", ui.PlaygroundPage)
		r.Get("/playground/models", ui.PlaygroundModels)
		r.Post("/settings/clear-stats", ui.ClearStats)
		r.Post("/settings/toggle-stats", ui.ToggleStats)
		r.Post("/settings/keys/add", ui.AddAPIKey)
		r.Post("/settings/keys/delete", ui.DeleteAPIKey)
		r.Post("/settings/backends/toggle", ui.ToggleBackend)
		r.Get("/stats/detail", ui.RequestDetail)
		r.Get("/quota", ui.QuotaFragment)
		r.Post("/settings/clients/add", ui.AddClient)
		r.Post("/settings/clients/delete", ui.DeleteClient)
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
			log.Fatalf("failed to create static sub-filesystem: %v", err)
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
			log.Println("received SIGHUP, reloading config...")
			if err := cfgMgr.Reload(); err != nil {
				log.Printf("config reload failed: %v", err)
			} else {
				log.Println("config reloaded successfully")
			}
		}
	}()

	go func() {
		log.Printf("starting server on %s", cfg.Server.Listen)
		log.Printf("  API:       http://localhost%s/v1/chat/completions", cfg.Server.Listen)
		log.Printf("  Dashboard: http://localhost%s/ui/", cfg.Server.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	go func() {
		log.Printf("starting Codex OAuth loopback callback server on %s", codexLoopbackListenAddr)
		if err := codexLoopbackSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("codex loopback callback server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
	if err := codexLoopbackSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("codex loopback shutdown error: %v", err)
	}
	if store != nil {
		store.Close()
	}
	log.Println("server stopped")
}
