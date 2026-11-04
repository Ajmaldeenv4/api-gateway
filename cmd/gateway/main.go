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

	"github.com/redis/go-redis/v9"

	"github.com/ajmal/api-gateway/internal/admin"
	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/healthcheck"
	"github.com/ajmal/api-gateway/internal/server"
	"github.com/ajmal/api-gateway/internal/store"
	"github.com/ajmal/api-gateway/internal/tracing"
)

const (
	dbConnectTimeout = 10 * time.Second
	shutdownTimeout  = 15 * time.Second
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to gateway.yaml")
	probe := flag.String("healthcheck", "", "GET this URL and exit 0 on success (the distroless image has no curl or wget)")
	flag.Parse()

	if *probe != "" {
		os.Exit(runProbe(*probe))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(*configPath, logger); err != nil {
		logger.Error("gateway stopped", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// OpenTelemetry — disabled unless OTEL_EXPORTER_OTLP_ENDPOINT is set.
	otlpEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	shutdownTracing, err := tracing.Init(ctx, "api-gateway", otlpEndpoint)
	if err != nil {
		logger.Warn("tracing init failed — continuing without traces", "err", err)
	} else {
		defer func() {
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = shutdownTracing(flushCtx)
		}()
	}

	// Redis client.
	var redisClient *redis.Client
	if cfg.Redis.Addr != "" {
		redisClient = redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		})
		defer func() { _ = redisClient.Close() }()
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := redisClient.Ping(pingCtx).Err(); err != nil {
			logger.Warn("redis unavailable at startup — rate limits will fail-open", "err", err)
		}
		cancel()
	}

	// Postgres, when configured, owns the route table. An unreachable database
	// is fatal: serving the YAML instead could revive routes deleted at runtime.
	var db *store.DB
	if dsn := os.Getenv("POSTGRES_DSN"); dsn != "" {
		db, err = openStore(ctx, dsn, cfg.Routes, logger)
		if err != nil {
			return err
		}
		defer db.Close()
		if cfg, err = loadConfig(ctx, configPath, db); err != nil {
			return err
		}
	}

	rt, err := server.NewRuntime(cfg, server.Options{
		RedisClient:   redisClient,
		HealthChecker: healthcheck.New(logger),
		OTelEnabled:   otlpEndpoint != "",
		Context:       ctx,
	}, logger)
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}
	defer rt.Close()

	reload := func(ctx context.Context) error {
		next, err := loadConfig(ctx, configPath, db)
		if err != nil {
			return err
		}
		return rt.Reload(next)
	}

	// SIGHUP re-reads routes. Listener settings need a restart.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go reloadOnSignal(ctx, hup, reload, logger)

	// Main gateway server.
	gw := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      rt,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Prometheus metrics on a separate port.
	metricsSrv := &http.Server{
		Addr:        cfg.Server.MetricsAddr,
		Handler:     server.MetricsHandler(),
		ReadTimeout: 5 * time.Second,
	}
	go serveBackground(metricsSrv, "metrics", logger)

	// Admin API on its own port, only when it can be secured.
	adminSrv := newAdminServer(cfg, db, reload, logger)
	if adminSrv != nil {
		go serveBackground(adminSrv, "admin", logger)
	}

	gwErr := make(chan error, 1)
	go func() {
		logger.Info("gateway listening",
			"addr", cfg.Server.Listen,
			"routes", len(cfg.Routes),
			"tracing", otlpEndpoint != "",
			"postgres", db != nil,
		)
		gwErr <- gw.ListenAndServe()
	}()

	select {
	case err := <-gwErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("gateway: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	logger.Info("shutting down gracefully")

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	_ = gw.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
	if adminSrv != nil {
		_ = adminSrv.Shutdown(shutCtx)
	}
	return nil
}

// reloadOnSignal runs reload for each signal until ctx ends. A failed reload is
// logged and the routes already running stay in place.
func reloadOnSignal(ctx context.Context, sig <-chan os.Signal, reload func(context.Context) error, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sig:
			if err := reload(ctx); err != nil {
				logger.Error("reload failed, keeping current routes", "err", err)
			}
		}
	}
}

// openStore connects, applies the schema, and seeds an empty database from the
// YAML routes, which is what makes that file the bootstrap source.
func openStore(ctx context.Context, dsn string, routes []config.Route, logger *slog.Logger) (*store.DB, error) {
	connectCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancel()
	db, err := store.Connect(connectCtx, dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(connectCtx); err != nil {
		db.Close()
		return nil, err
	}
	seeded, err := db.Seed(connectCtx, routes)
	if err != nil {
		db.Close()
		return nil, err
	}
	if seeded > 0 {
		logger.Info("seeded route store from config file", "routes", seeded)
	}
	return db, nil
}

// loadConfig reads the YAML file and, with a store, replaces its routes with
// the stored ones.
func loadConfig(ctx context.Context, path string, db *store.DB) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return cfg, nil
	}
	routes, err := db.LoadRoutes(ctx)
	if err != nil {
		return nil, err
	}
	if err := config.ValidateRoutes(routes); err != nil {
		return nil, fmt.Errorf("stored routes: %w", err)
	}
	cfg.Routes = routes
	return cfg, nil
}

func newAdminServer(cfg *config.Config, db *store.DB, reload func(context.Context) error, logger *slog.Logger) *http.Server {
	if cfg.Server.AdminAddr == "" {
		return nil
	}
	if db == nil {
		logger.Warn("admin API disabled: it needs POSTGRES_DSN")
		return nil
	}
	token := os.Getenv("ADMIN_TOKEN")
	if token == "" {
		logger.Warn("admin API disabled: ADMIN_TOKEN is not set")
		return nil
	}
	return &http.Server{
		Addr:        cfg.Server.AdminAddr,
		Handler:     admin.New(db, token, reload, logger).Handler(),
		ReadTimeout: 10 * time.Second,
	}
}

func serveBackground(srv *http.Server, name string, logger *slog.Logger) {
	logger.Info(name+" server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Error(name+" server error", "err", err)
	}
}

// runProbe backs the container health check.
func runProbe(url string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url) //nolint:gosec // URL comes from a trusted CLI flag, not user input
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		fmt.Fprintln(os.Stderr, resp.Status)
		return 1
	}
	return 0
}
