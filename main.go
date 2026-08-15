// Command freebuff-proxy (root entrypoint) — Vercel deployment mirror.
//
// Vercel's Go Framework Preset requires a root-level entrypoint
// (main.go / cmd/api/main.go / cmd/server/main.go) before it builds. The
// upstream entrypoint lives at cmd/freebuff-proxy/main.go, which is not in
// that list, so this root main.go is provided as the Vercel build target.
//
// It is a minimal mirror of the upstream entrypoint: config loading, model
// registry (fallback + background refresh), the per-token upstream clients /
// session managers / run managers bound into the token pool, and the
// OpenAI-compatible HTTP surface over it, with graceful SIGINT/SIGTERM
// shutdown. The interactive subcommands (-doctor, -update, -setup) are
// deliberately omitted — they have no meaning on a serverless container.
//
// Keep the runtime behavior in sync with cmd/freebuff-proxy/main.go when
// upstream changes the startup sequence.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/pool"
	"freebuff-proxy/internal/registry"
	"freebuff-proxy/internal/server"
	"freebuff-proxy/internal/session"
	"freebuff-proxy/internal/telemetry"
	"freebuff-proxy/internal/upstream"
)

// version is injected at build time by GoReleaser (-ldflags -X main.version=...).
// When building without GoReleaser it stays "dev".
var version = "dev"

func main() {
	configPath := flag.String("config", "", "path to an optional JSON config file (keys mirror env names)")
	verbose := flag.Bool("v", false, "verbose (debug) logging")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("freebuff-proxy", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "freebuff-proxy: invalid config:", err)
		os.Exit(1)
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else info.
	level, _ := telemetry.ParseLevel(cfg.LogLevel)
	if cfg.LogLevel == "" {
		if *verbose {
			level = slog.LevelDebug
		} else {
			level = slog.LevelInfo
		}
	}
	logger := telemetry.New(level, cfg.LogFile)
	// The pool/upstream/session/runs log through slog.Default(); route it
	// through our logger so the configured level and log file cover them too.
	slog.SetDefault(logger)

	// Load the hardcoded fallback immediately so the registry is usable
	// offline; the first background refresh replaces it on success.
	reg := registry.New(&cfg, &http.Client{Timeout: 30 * time.Second})
	reg.LoadFallback()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go refreshLoop(ctx, logger, reg, cfg.RegistryRefresh)

	// One upstream client and session manager per token, bound into the pool
	// together with a per-token run manager.
	clients := make([]*upstream.Client, 0, len(cfg.AuthTokens))
	sessions := make([]*session.Manager, 0, len(cfg.AuthTokens))
	for i, token := range cfg.AuthTokens {
		client, err := upstream.NewWithIndex(token, i, &cfg)
		if err != nil {
			logger.Error("failed to build upstream client", "err", err)
			os.Exit(1)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManager(client))
	}
	if cfg.DiscoveredSource != "" {
		logger.Info("auto-discovered FreeBuff token from CLI login", "email", cfg.DiscoveredEmail, "file", cfg.DiscoveredSource)
	}
	p, err := pool.New(&cfg, clients, sessions, reg)
	if err != nil {
		logger.Error("failed to build pool", "err", err)
		os.Exit(1)
	}

	// Prewarm + the maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	srv := server.New(&cfg, p, reg, logger)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	// Startup summary -- token values are never logged, only counts.
	logger.Info("freebuff-proxy starting",
		"version", version,
		"listen_addr", cfg.ListenAddr,
		"upstream", cfg.UpstreamBaseURL,
		"auth_tokens", len(cfg.AuthTokens),
		"bridge_mode", len(cfg.AuthTokens) == 0,
		"api_keys", len(cfg.APIKeys),
		"cost_mode", cfg.CostMode,
		"rotation_interval", cfg.RotationInterval.String(),
		"registry_refresh", cfg.RegistryRefresh.String(),
		"registry_agents", len(reg.AgentIDs()),
		"registry_models", reg.ModelCount(),
		"log_level", level.String(),
		"verbose", *verbose,
	)

	// Serve until the server fails or a shutdown signal arrives.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			stop() // cancel ctx: stop the pool jobs, then drain
		}
	case <-ctx.Done():
	}

	// Graceful drain: stop accepting new requests first, then finish
	// runs/sessions, bounded by a 10s force deadline.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown incomplete", "err", err)
	}
	p.Shutdown(shutdownCtx)
	logger.Info("shutdown complete")
}

// refreshLoop refreshes the registry immediately, then every interval.
// Refresh failures keep the previous state (the fallback at boot); the next
// tick retries.
func refreshLoop(ctx context.Context, logger *slog.Logger, reg *registry.Registry, interval time.Duration) {
	logRegistryRefresh(ctx, logger, reg)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logRegistryRefresh(ctx, logger, reg)
		}
	}
}

func logRegistryRefresh(ctx context.Context, logger *slog.Logger, reg *registry.Registry) {
	if err := reg.Refresh(ctx); err != nil {
		logger.Warn("registry refresh failed; keeping previous state", "err", err)
		return
	}
	logger.Info("registry refreshed", "agents", len(reg.AgentIDs()), "models", reg.ModelCount())
}
