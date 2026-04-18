package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/healthcheck"
	"github.com/ajmal/api-gateway/internal/server"
	"github.com/ajmal/api-gateway/internal/tracing"
)

func main() {
	configPath := flag.String("config", "configs/gateway.yaml", "path to gateway.yaml")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("failed to load config", "err", err)
		os.Exit(1)
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
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		if err := redisClient.Ping(pingCtx).Err(); err != nil {
			logger.Warn("redis unavailable at startup — rate limits will fail-open", "err", err)
		}
		cancel()
	}

	// Active health checker.
	hc := healthcheck.New(logger)

	opts := server.Options{
		RedisClient:   redisClient,
		HealthChecker: hc,
		OTelEnabled:   otlpEndpoint != "",
	}

	handler, err := server.Build(cfg, opts, logger)
	if err != nil {
		logger.Error("failed to build server", "err", err)
		os.Exit(1)
	}

	// Main gateway server.
	gw := &http.Server{
		Addr:         cfg.Server.Listen,
		Handler:      handler,
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

	go func() {
		logger.Info("metrics server listening", "addr", cfg.Server.MetricsAddr)
		if err := metricsSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "err", err)
		}
	}()

	go func() {
		logger.Info("gateway listening",
			"addr", cfg.Server.Listen,
			"routes", len(cfg.Routes),
			"tracing", otlpEndpoint != "",
		)
		if err := gw.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			logger.Error("gateway error", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down gracefully")

	shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = gw.Shutdown(shutCtx)
	_ = metricsSrv.Shutdown(shutCtx)
}
