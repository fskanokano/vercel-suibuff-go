// Command freebuff-proxy (root entrypoint) — Vercel deployment mirror.
//
// Vercel's documented Go support is handler-based serverless functions
// (@vercel/go), not a persistent port-listening binary. This root main.go is
// the target of vercel.json's buildCommand ("go build -o server .") so the
// build produces a standalone binary; whether Vercel actually runs that
// binary as a long-lived server has NOT been verified with a real deploy —
// treat the deployment model as experimental until confirmed. The upstream
// entrypoint lives at cmd/freebuff-proxy/main.go.
//
// It is a mirror of the upstream entrypoint: config loading, the model
// registry (fallback + background refresh), the per-token upstream clients /
// session managers / run managers bound into the token pool, the in-memory
// log ring that backs the admin dashboard, and the OpenAI-compatible HTTP
// surface over it, with graceful SIGINT/SIGTERM shutdown. The interactive
// subcommands (-doctor, -update, -setup, -test-token, -install-service, …)
// and the interactive console banner are deliberately omitted — they have no
// meaning on a serverless container. The dashboard login wizard and the
// release-update badge are likewise omitted (both write/poll state that is
// meaningless on Vercel's read-only, ephemeral filesystem).
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
	"strings"
	"syscall"
	"time"

	// Embed the IANA tzdata so NextPacificMidnight keeps exact DST math on
	// minimal images (alpine:3.20 has no /usr/share/zoneinfo) and hosts
	// without timezone registry entries. Without this, Pacific resets fall
	// back to a month-based approximation.
	_ "time/tzdata"

	"freebuff-proxy/internal/config"
	"freebuff-proxy/internal/egressip"
	"freebuff-proxy/internal/logring"
	"freebuff-proxy/internal/notify"
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

	// Vercel / generic container runtimes inject the per-instance listen
	// port via PORT (a bare number, e.g. "44465"). The platform health check
	// dials exactly that port, so prefer it over LISTEN_ADDR whenever PORT is
	// present; LISTEN_ADDR remains the fallback for local / self-hosted runs.
	if port := strings.TrimSpace(os.Getenv("PORT")); port != "" {
		if port = strings.TrimPrefix(port, ":"); port != "" {
			cfg.ListenAddr = ":" + port
		}
	}

	// Effective log level: LOG_LEVEL config wins, else -v → debug, else info.
	level := resolveLogLevel(cfg.LogLevel, *verbose)
	logger := telemetry.New(level, cfg.LogFile, cfg.LogFormat)
	// The dashboard log viewer reads from an in-memory ring that mirrors
	// every record the process logger emits (no log file or docker needed).
	logringHandler := logring.NewHandler(logger.Handler(), cfg.LogRingSize)
	logger = slog.New(logringHandler)
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
	// together with a per-token run manager. When SESSION_PERSIST is enabled
	// one shared store backs every session manager, so a restart resumes
	// unexpired sessions (on Vercel the ephemeral FS resets this on cold start).
	var store *session.Store
	if cfg.SessionPersist {
		store = session.NewStore(cfg.SessionStateFile)
		logger.Info("session state persistence enabled", "file", cfg.SessionStateFile)
	}
	clients := make([]*upstream.Client, 0, len(cfg.AuthTokens))
	sessions := make([]*session.Manager, 0, len(cfg.AuthTokens))
	for i, token := range cfg.AuthTokens {
		client, err := upstream.NewWithIndex(token, i, &cfg)
		if err != nil {
			logger.Error("failed to build upstream client", "err", err)
			os.Exit(1)
		}
		clients = append(clients, client)
		sessions = append(sessions, session.NewManagerWithStore(client, store))
	}
	if cfg.DiscoveredSource != "" {
		logger.Info("auto-discovered FreeBuff token from CLI login", "email", cfg.DiscoveredEmail, "file", cfg.DiscoveredSource)
	}
	p, err := pool.New(&cfg, clients, sessions, reg)
	if err != nil {
		logger.Error("failed to build pool", "err", err)
		os.Exit(1)
	}
	p.SetSessionStore(store)

	// Best-effort webhook alerts (WEBHOOK_URL) for pool exhaustion / token
	// bans — fire-and-forget, throttled, never blocking. This works on
	// Vercel (a plain outbound HTTPS POST), so it is kept in the mirror.
	if cfg.WebhookURL != "" {
		p.SetNotifier(notify.New(cfg.WebhookURL, nil))
		logger.Info("webhook alerts enabled", "url", cfg.WebhookURL)
	}

	// Prewarm + the maintain loop run until ctx is canceled (shutdown).
	p.Start(ctx)

	// Egress probing is intentionally NOT wired into startup (mirrors
	// upstream #123): the official CLI never probes cloudflare.com, and the
	// background loop's risk-engine feed has no consumer. The probe still
	// runs on demand via `-doctor`, which is omitted here (no interactive
	// subcommands on a container).

	srv := server.New(&cfg, p, reg, logger, logringHandler, *configPath)
	// Egress-IP endpoint (public GET /egress/ip): reports this instance's
	// outbound public IP and its location by rotating through several free
	// IP-geolocation services. Wired here — not in the server route table —
	// so the feature stays fully isolated from upstream-synced code.
	egressClient := egressip.NewClient(logger)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           egressip.Wrap(srv.Handler(), egressClient),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		// IdleTimeout closes keep-alive connections that have been idle for
		// two minutes, bounding goroutines parked on dead clients.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is deliberately unset (0): /v1/chat/completions
		// streams SSE responses that can legitimately outlive any fixed
		// write budget.
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
		"log_level", logLevelDisplay(level),
		"verbose", *verbose,
	)
	// /admin/reload and the admin dashboard are open in default deployments
	// (no API_KEYS, or bridge mode). On Vercel the proxy is public-reachable,
	// so this warning is especially important: set ADMIN_TOKEN.
	if cfg.AdminToken == "" && (len(cfg.APIKeys) == 0 || cfg.BridgeMode()) {
		logger.Warn("/admin/reload and the /admin dashboard are unauthenticated — any client that can reach the proxy can reload configuration and view its state. Set ADMIN_TOKEN to require a bearer token")
	}
	logger.Info("listening", "addr", cfg.ListenAddr)

	// Serve until the server fails or a shutdown signal arrives.
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()

	// A failed bind (port already in use) is the most common startup error.
	// Mirror upstream: any server failure exits non-zero after the drain so
	// health checks can tell the process never came up instead of reading a
	// clean exit 0.
	exitCode := 0
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "err", err)
			exitCode = 1
			stop() // cancel ctx: stop the pool jobs, then drain
		}
	case <-ctx.Done():
	}

	// Graceful drain: stop accepting new requests first, then finish
	// runs/sessions. HTTP gets a 10s force deadline; the pool then gets its
	// OWN fresh budget — a slow-draining SSE stream can consume the whole
	// HTTP budget, and the pool drain must not be starved by it.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown incomplete", "err", err)
	}
	poolCtx, poolCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer poolCancel()
	p.Shutdown(poolCtx)
	logger.Info("shutdown complete")
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// resolveLogLevel applies the effective log-level precedence: a set
// LOG_LEVEL config wins, -v → debug, else info. An unparseable LOG_LEVEL
// silently falls back to info (ParseLevel returns ok=false).
func resolveLogLevel(cfgLogLevel string, verbose bool) slog.Level {
	if cfgLogLevel != "" {
		if lv, ok := telemetry.ParseLevel(cfgLogLevel); ok {
			return lv
		}
		return slog.LevelInfo
	}
	if verbose {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// logLevelDisplay renders the configured level for the startup summary.
// LevelTrace prints as TRACE instead of slog's "DEBUG-4".
func logLevelDisplay(level slog.Level) string {
	if level == telemetry.LevelTrace {
		return "TRACE"
	}
	return level.String()
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
	}
}
